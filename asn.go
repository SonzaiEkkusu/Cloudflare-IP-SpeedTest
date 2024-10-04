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
	asnList     = flag.String("asn", "", "Comma-separated ASN numbers")
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
			fmt.Printf("Failed to retrieve information for ASN %s: %v\n", asn, err)
			continue
		}

		outFile := asnInfo.Name + ".csv"

		fmt.Printf("ASN information: %s\n", asn)
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
		return ASNInfo{}, fmt.Errorf("failed to retrieve ASN information: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ASNInfo{}, fmt.Errorf("failed to retrieve ASN information: received status code %d", resp.StatusCode)
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
			return nil, fmt.Errorf("failed to get JSON from URL: %v", err)
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
		return fmt.Errorf("failed to remove existing file: %v", err)
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
			fmt.Printf("Failed to generate IPs for CIDR %s: %v\n", cidrBlock, err)
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
			return nil, fmt.Errorf("failed to retrieve CIDR blocks: %v", err)
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
			fmt.Printf("Request limit exceeded, retrying after %v...\n", retryAfter)
			time.Sleep(retryAfter)
			continue
		}

		return nil, fmt.Errorf("failed to retrieve CIDR blocks: received status code %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("maximum number of attempts to retrieve CIDR blocks exceeded")
}

func calculateTotalIPs(cidrBlocks []string) (int, error) {
	var totalIPs int
	for _, cidr := range cidrBlocks {
		count, err := countIPsInCIDR(cidr)
		if err != nil {
			fmt.Printf("Failed to count IPs in CIDR %s: %v\n", cidr, err)
			continue
		}
		totalIPs += count
	}
	return totalIPs, nil
}

func countIPsInCIDR(cidr string) (int, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0, fmt.Errorf("failed to parse CIDR: %v", err)
	}
	ones, bits := ipNet.Mask.Size()
	return 1 << (bits - ones), nil
}

func generateIPs(cidr string) ([]string, error) {
	var ips []string
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CIDR: %v", err)
	}

	for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}
	return ips, nil
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func processIPs(ips []string, locationMap map[string]location, totalIPs int, processedIPs *int, lock *sync.Mutex) []result {
	var wg sync.WaitGroup
	resultChan := make(chan result, len(ips))
	thread := make(chan struct{}, *maxThreads)

	for _, ip := range ips {
		thread <- struct{}{}
		wg.Add(1)
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				updateProgress(processedIPs, totalIPs, lock)
			}()

			if res, err := processIP(ip, locationMap); err == nil {
				resultChan <- res
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	results := make([]result, 0, len(resultChan))
	for res := range resultChan {
		results = append(results, res)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].tcpDuration < results[j].tcpDuration
	})
	return results
}

func processIP(ip string, locationMap map[string]location) (result, error) {
	dialer := &net.Dialer{
		Timeout: timeout,
	}
	start := time.Now()
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(*defaultPort)))
	if err != nil {
		return result{}, err
	}
	defer conn.Close()

	tcpDuration := time.Since(start)
	start = time.Now()

	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
		},
		Timeout: timeout,
	}

	protocol := "http://"
	if *enableTLS {
		protocol = "https://"
	}
	reqURL := protocol + requestURL

	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		return result{}, err
	}
	defer resp.Body.Close()

	duration := time.Since(start)
	if duration > maxDuration {
		return result{}, fmt.Errorf("the request took too long")
	}

	buf := &bytes.Buffer{}
	timeoutChan := time.After(maxDuration)
	done := make(chan bool)
	go func() {
		_, err := io.Copy(buf, resp.Body)
		done <- true
		if err != nil {
			return
		}
	}()
	select {
	case <-done:
	case <-timeoutChan:
		return result{}, fmt.Errorf("the request timed out")
	}

	body := buf
	if err != nil {
		return result{}, err
	}

	return parseResult(body, ip, tcpDuration, locationMap)
}

func parseResult(body *bytes.Buffer, ip string, tcpDuration time.Duration, locationMap map[string]location) (result, error) {
	if strings.Contains(body.String(), "uag=Mozilla/5.0") {
		if matches := regexp.MustCompile(`colo=([A-Z]+)`).FindStringSubmatch(body.String()); len(matches) > 1 {
			dataCenter := matches[1]
			loc, ok := locationMap[dataCenter]
			if ok {
				fmt.Printf("Valid IP %s, location %s, latency %d ms\n", ip, loc.City, tcpDuration.Milliseconds())
				return result{ip, *defaultPort, dataCenter, loc.Region, loc.City, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}, nil
			}
			fmt.Printf("Valid IP %s, unknown location, latency %d ms\n", ip, tcpDuration.Milliseconds())
			return result{ip, *defaultPort, dataCenter, "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}, nil
		}
	}
	return result{}, fmt.Errorf("failed to parse the result")
}

func updateProgress(processedIPs *int, totalIPs int, lock *sync.Mutex) {
	lock.Lock()
	defer lock.Unlock()
	*processedIPs++
	percentage := float64(*processedIPs) / float64(totalIPs) * 100
	fmt.Printf("Completed: %d out of %d IP addresses (%.2f%%)\r", *processedIPs, totalIPs, percentage)
	if *processedIPs == totalIPs {
		fmt.Printf("Completed: %d out of %d IP addresses (%.2f%%)\n", *processedIPs, totalIPs, percentage)
	}
}

func sortResultsByDuration(results []result) {
	sort.Slice(results, func(i, j int) bool {
		return results[i].tcpDuration < results[j].tcpDuration
	})
}

func isFileEmpty(filename string) (bool, error) {
	info, err := os.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return info.Size() == 0, nil
}

func writeResults(results []result, outFile string, appendToFile bool) error {
	if len(results) == 0 {
		return nil
	}

	file, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if fileInfo, err := file.Stat(); err == nil && fileInfo.Size() == 0 {
		writer.Write([]string{"IP Address", "Port", "TLS", "Data Center", "Region", "City", "Latency"})
	}

	for _, res := range results {
		writer.Write([]string{res.ip, strconv.Itoa(res.port), strconv.FormatBool(*enableTLS), res.dataCenter, res.region, res.city, res.latency})
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
	cmd.Stdout = os.Stdout
	cmd.Run()
}
