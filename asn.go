package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	requestURL  = "speed.cloudflare.com/cdn-cgi/trace"
	timeout     = 1 * time.Second
	maxDuration = 2 * time.Second
	batchSize   = 1000
)

var (
	asnList     = flag.String("asn", "", "ASN numbers separated by commas")
	defaultPort = flag.Int("port", 443, "Port")
	maxThreads  = flag.Int("max", 50, "Maximum number of parallel requests")
	enableTLS   = flag.Bool("tls", true, "Enable TLS")
)

type result struct {
	ip          string
	port        int
	dataCenter  string
	region      string
	city        string
	latency     string
	tcpDuration time.Duration
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

type CIDRBlock struct {
	Prefix string `json:"prefix"`
}

type ASNInfo struct {
	Name        string `json:"name"`
	CountryCode string `json:"country_code"`
}

func main() {
	flag.Parse()
	if *asnList == "" {
		fmt.Println("ASN number is required")
		return
	}
	asns := strings.Split(*asnList, ",")

	for _, asn := range asns {
		asn := strings.TrimSpace(asn)
		if asn == "" {
			continue
		}

		clearConsole()
		startTime := time.Now()

		asnInfo, err := getASNInfo(asn)
		if err != nil {
			fmt.Printf("Failed to get ASN information for %s: %v\n", asn, err)
			continue
		}

		outFile := asnInfo.Name + ".csv"

		fmt.Printf("ASN Information: %s\n", asn)
		fmt.Printf("  Name: %s\n", asnInfo.Name)
		fmt.Printf("  Country: %s\n", asnInfo.CountryCode)

		locations, err := loadLocations()
		if err != nil {
			fmt.Printf("Failed to load locations: %v\n", err)
			continue
		}

		locationMap := createLocationMap(locations)

		if err := prepareOutputFile(outFile); err != nil {
			fmt.Printf("Failed to prepare output file: %v\n", err)
			continue
		}

		validIPCount, err := processIPsFromASN(asn, locationMap, batchSize, outFile)
		if err != nil {
			fmt.Printf("Failed to process IP addresses for ASN %s: %v\n", asn, err)
			continue
		}

		elapsed := time.Since(startTime)
		if validIPCount == 0 {
			fmt.Printf("This ASN has no valid IPs\n")
		} else {
			fmt.Printf("Results successfully written to %s, time taken: %s\n", outFile, formatDuration(elapsed))
		}
	}
}

func getASNInfo(asn string) (ASNInfo, error) {
	url := fmt.Sprintf("https://api.bgpview.io/asn/%s", asn)
	resp, err := http.Get(url)
	if err != nil {
		return ASNInfo{}, fmt.Errorf("failed to get ASN information: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ASNInfo{}, fmt.Errorf("failed to get ASN information: received status code %d", resp.StatusCode)
	}

	var response struct {
		Data ASNInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return ASNInfo{}, fmt.Errorf("failed to parse response: %v", err)
	}

	return response.Data, nil
}

func loadLocations() ([]location, error) {
	var locations []location

	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("Local file locations.json not found, downloading...")
		resp, err := http.Get("https://speed.cloudflare.com/locations")
		if err != nil {
			return nil, fmt.Errorf("failed to fetch JSON from URL: %v", err)
		}
		defer resp.Body.Close()

		if err := json.NewDecoder(resp.Body).Decode(&locations); err != nil {
			return nil, fmt.Errorf("failed to parse JSON: %v", err)
		}

		file, err := os.Create("locations.json")
		if err != nil {
			return nil, fmt.Errorf("failed to create file: %v", err)
		}
		defer file.Close()

		if err := json.NewEncoder(file).Encode(locations); err != nil {
			return nil, fmt.Errorf("failed to write JSON to file: %v", err)
		}
	} else {
		fmt.Println("Local file locations.json found, loading...")
		file, err := os.Open("locations.json")
		if err != nil {
			return nil, fmt.Errorf("failed to read file: %v", err)
		}
		defer file.Close()

		if err := json.NewDecoder(file).Decode(&locations); err != nil {
			return nil, fmt.Errorf("failed to parse JSON: %v", err)
		}
	}

	return locations, nil
}

func createLocationMap(locations []location) map[string]location {
	locationMap := make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}
	return locationMap
}

func prepareOutputFile(outFile string) error {
	if err := os.Remove(outFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete existing file: %v", err)
	}
	return nil
}

func processIPsFromASN(asn string, locationMap map[string]location, batchSize int, outFile string) (int, error) {
	fmt.Printf("Processing ASN: %s\n", asn)

	cidrBlocks, err := fetchCIDRBlocksFromASN(asn)
	if err != nil {
		return 0, err
	}

	fmt.Printf("Total CIDR blocks: %d\n", len(cidrBlocks))

	totalIPs, err := calculateTotalIPs(cidrBlocks)
	if err != nil {
		return 0, err
	}

	fmt.Printf("Total IP addresses: %d\n", totalIPs)

	var processedIPs int
	var validIPCount int
	var lock sync.Mutex

	for _, cidrBlock := range cidrBlocks {
		ips, err := generateIPs(cidrBlock)
		if err != nil {
			fmt.Printf("Failed to generate IP for CIDR %s: %v\n", cidrBlock, err)
			continue
		}

		for len(ips) > 0 {
			batch := ips
			if len(ips) > batchSize {
				batch = ips[:batchSize]
				ips = ips[batchSize:]
			} else {
				ips = nil
			}

			results := processIPs(batch, locationMap, totalIPs, &processedIPs, &lock)
			if len(results) > 0 {
				validIPCount += len(results)
				if err := writeResults(results, outFile, processedIPs != batchSize); err != nil {
					return validIPCount, err
				}
			}
		}
	}

	return validIPCount, nil
}

func fetchCIDRBlocksFromASN(asn string) ([]string, error) {
	url := fmt.Sprintf("https://api.bgpview.io/asn/%s/prefixes", asn)
	for attempts := 0; attempts < 5; attempts++ {
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch CIDR blocks: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var response struct {
				Data struct {
					IPv4Prefixes []CIDRBlock `json:"ipv4_prefixes"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
				return nil, fmt.Errorf("failed to parse response: %v", err)
			}

			cidrBlocks := make([]string, len(response.Data.IPv4Prefixes))
			for i, prefix := range response.Data.IPv4Prefixes {
				cidrBlocks[i] = prefix.Prefix
			}
			return cidrBlocks, nil
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := time.Second * 2
			if retryAfterHeader := resp.Header.Get("Retry-After"); retryAfterHeader != "" {
				if retryAfterSeconds, err := strconv.Atoi(retryAfterHeader); err == nil {
					retryAfter = time.Duration(retryAfterSeconds) * time.Second
				}
			}
			fmt.Printf("Rate limit exceeded, retrying in %v...\n", retryAfter)
			time.Sleep(retryAfter)
			continue
		}

		return nil, fmt.Errorf("failed to fetch CIDR blocks: received status code %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("failed to fetch CIDR blocks after multiple attempts")
}

func calculateTotalIPs(cidrBlocks []string) (int, error) {
	var totalIPs int
	for _, cidr := range cidrBlocks {
		ip, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return 0, fmt.Errorf("failed to parse CIDR %s: %v", cidr, err)
		}
		ones, bits := ipNet.Mask.Size()
		ipCount := 1 << (bits - ones)
		totalIPs += ipCount
		fmt.Printf("CIDR: %s has %d IPs\n", ip.String(), ipCount)
	}
	return totalIPs, nil
}

func generateIPs(cidr string) ([]string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CIDR %s: %v", cidr, err)
	}

	var ips []string
	for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); incrementIP(ip) {
		ips = append(ips, ip.String())
	}

	// Remove network and broadcast addresses
	if len(ips) > 2 {
		ips = ips[1 : len(ips)-1]
	}

	return ips, nil
}

func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func processIPs(ips []string, locationMap map[string]location, totalIPs int, processedIPs *int, lock *sync.Mutex) []result {
	var results []result
	var wg sync.WaitGroup
	sem := make(chan struct{}, *maxThreads)

	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			dataCenter, region, city := fetchIPInfo(ip, locationMap)
			if dataCenter == "" {
				return
			}

			tcpDuration, latency := measureLatency(ip)

			lock.Lock()
			*processedIPs++
			fmt.Printf("Processing IP %d/%d: %s (%s, %s, %s)\n", *processedIPs, totalIPs, ip, dataCenter, region, city)
			lock.Unlock()

			results = append(results, result{
				ip:          ip,
				port:        *defaultPort,
				dataCenter:  dataCenter,
				region:      region,
				city:        city,
				latency:     latency,
				tcpDuration: tcpDuration,
			})
		}(ip)
	}
	wg.Wait()

	return results
}

func fetchIPInfo(ip string, locationMap map[string]location) (string, string, string) {
	// Implement function to fetch IP info using the location map
	// Placeholder return for now
	return "ExampleDataCenter", "ExampleRegion", "ExampleCity"
}

func measureLatency(ip string) (time.Duration, string) {
	// Placeholder function for measuring latency
	return 100 * time.Millisecond, "100 ms"
}

func writeResults(results []result, outFile string, appendToFile bool) error {
	file, err := os.OpenFile(outFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %v", outFile, err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if !appendToFile {
		writer.Write([]string{"IP", "Port", "DataCenter", "Region", "City", "Latency", "TCP Duration"})
	}

	for _, res := range results {
		writer.Write([]string{
			res.ip,
			strconv.Itoa(res.port),
			res.dataCenter,
			res.region,
			res.city,
			res.latency,
			fmt.Sprintf("%v", res.tcpDuration),
		})
	}

	return nil
}
func formatDuration(d time.Duration) string {
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	} else if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	} else {
		return fmt.Sprintf("%ds", s)
	}
}

func clearConsole() {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "cls")
	case "linux", "darwin":
		cmd = exec.Command("clear")
	default:
		cmd = exec.Command("clear")
	}
	cmd.Stdout= os.Stdout
	cmd.Run()
}
