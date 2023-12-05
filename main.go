
package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
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
	requestURL  = "speed.cloudflare.com/cdn-cgi/trace" // Trace URL for requests
	timeout     = 1 * time.Second                      // Timeout duration
	maxDuration = 2 * time.Second                      // Maximum duration
)

var (
	File         = flag.String("file", "ip.txt", "IP address file name")                                     // IP address file name
	outFile      = flag.String("outfile", "ip.csv", "Output file name")                                     // Output file name
	defaultPort  = flag.Int("port", 443, "Port")                                                            // Port
	maxThreads   = flag.Int("max", 100, "Maximum concurrent goroutines for requests")                        // Maximum goroutines
	speedTest    = flag.Int("speedtest", 5, "Number of download speed test goroutines, set to 0 to disable") // Number of download speed test goroutines
	speedTestURL = flag.String("url", "speed.cloudflare.com/__down?bytes=500000000", "Speed test file URL")   // Speed test file URL
	enableTLS    = flag.Bool("tls", true, "Enable TLS")                                                     // Enable TLS
)

type result struct {
	ip          string        // IP address
	port        int           // Port
	dataCenter  string        // Data center
	region      string        // Region
	city        string        // City
	latency     string        // Latency
	tcpDuration time.Duration // TCP request latency
}

type speedtestresult struct {
	result
	downloadSpeed float64 // Download speed
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

// Attempt to increase the file descriptor limit
func increaseMaxOpenFiles() {
	fmt.Println("Attempting to increase file descriptor limit...")
	cmd := exec.Command("bash", "-c", "ulimit -n 10000")
	_, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Error increasing file descriptor limit: %v\n", err)
	} else {
		fmt.Printf("File descriptor limit increased!\n")
	}
}

func main() {
	flag.Parse()

	startTime := time.Now()
	osType := runtime.GOOS
	if osType == "linux" {
		increaseMaxOpenFiles()
	}

	var locations []location
	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("Local locations.json does not exist\nDownloading locations.json from https://speed.cloudflare.com/locations")
		resp, err := http.Get("https://speed.cloudflare.com/locations")
		if err != nil {
			fmt.Printf("Unable to fetch JSON from URL: %v\n", err)
			return
		}

		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("Unable to read response body: %v\n", err)
			return
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("Unable to parse JSON: %v\n", err)
			return
		}
		file, err := os.Create("locations.json")
		if err != nil {
			fmt.Printf("Unable to create file: %v\n", err)
			return
		}
		defer file.Close()

		_, err = file.Write(body)
		if err != nil {
			fmt.Printf("Unable to write to file: %v\n", err)
			return
		}
	} else {
		fmt.Println("Local locations.json already exists, no need to redownload")
		file, err := os.Open("locations.json")
		if err != nil {
			fmt.Printf("Unable to open file: %v\n", err)
			return
		}
		defer file.Close()

		body, err := ioutil.ReadAll(file)
		if err != nil {
			fmt.Printf("Unable to read file: %v\n", err)
			return
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("Unable to parse JSON: %v\n", err)
			return
		}
	}

	locationMap := make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}

	ips, err := readIPs(*File)
	if err != nil {
		fmt.Printf("Unable to read IPs from file: %v\n", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(ips))

	resultChan := make(chan result, len(ips))

	thread := make(chan struct{}, *maxThreads)

	var count int
	total := len(ips)

	for _, ip := range ips {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				count++
				percentage := float64(count) / float64(total) * 100
				fmt.Printf("Completed: %d Total: %d Percentage: %.2f%%\r", count, total, percentage)
				if count == total {
					fmt.Printf("Completed: %d Total: %d Percentage: %.2f%%\n", count, total, percentage)
				}
			}()

			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(*defaultPort)))
			if err != nil {
				return
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

			var protocol string
			if *enableTLS {
				protocol = "https://"
			} else {
				protocol = "http://"
			}
			requestURL := protocol + requestURL

			req, _ := http.NewRequest("GET", requestURL, nil)

			// Add user agent
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true
			resp, err := client.Do(req)
			if err != nil {
				return
			}

			duration := time.Since(start)
			if duration > maxDuration {
				return
			}

			buf := &bytes.Buffer{}
			// Create a timeout for read operation
			timeout := time.After(maxDuration)
			// Use a goroutine to read the response body
			done := make(chan bool)
			go func() {
				_, err := io.Copy(buf, resp.Body)
				done <- true
				if err != nil {
					return
				}
			}()

			// Wait for the read operation to complete or timeout
			select {
			case <-done:
				// Read operation completed
			case <-timeout:
				// Read operation timed out
				return
			}

			body := buf
			if err != nil {
				return
			}

			if strings.Contains(body.String(), "uag=Mozilla/5.0") {
				if matches := regexp.MustCompile(`colo=([A-Z]+)`).FindStringSubmatch(body.String()); len(matches) > 1 {
					dataCenter := matches[1]
					loc, ok := locationMap[dataCenter]
					if ok {
						fmt.Printf("Found valid IP %s at location %s with latency %d milliseconds\n", ip, loc.City, tcpDuration.Milliseconds())
						resultChan <- result{ip, *defaultPort, dataCenter, loc.Region, loc.City, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}
					} else {
						fmt.Printf("Found valid IP %s at unknown location with latency %d milliseconds\n", ip, tcpDuration.Milliseconds())
						resultChan <- result{ip, *defaultPort, dataCenter, "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}
					}
				}
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		// Clear output
		fmt.Print("\033[2J")
		fmt.Println("No valid IP found")
		return
	}
	var results []speedtestresult
	if *speedTest > 0 {
		fmt.Printf("Starting speed test\n")
		var wg2 sync.WaitGroup
		wg2.Add(*speedTest)
		count = 0
		total := len(resultChan)
		results = []speedtestresult{}
		for i := 0; i < *speedTest; i++ {
			thread <- struct{}{}
			go func() {
				defer func() {
					<-thread
					wg2.Done()
				}()
				for res := range resultChan {
					downloadSpeed := getDownloadSpeed(res.ip)
					results = append(results, speedtestresult{result: res, downloadSpeed: downloadSpeed})

					count++
					percentage := float64(count) / float64(total) * 100
					fmt.Printf("Completed: %.2f%%\r", percentage)
					if count == total {
						fmt.Printf("Completed: %.2f%%\033[0\n", percentage)
					}
				}
			}()
		}
		wg2.Wait()
	} else {
		for res := range resultChan {
			results = append(results, speedtestresult{result: res})
		}
	}

	if *speedTest > 0 {
		sort.Slice(results, func(i, j int) bool {
			return results[i].downloadSpeed > results[j].downloadSpeed
		})
	} else {
		sort.Slice(results, func(i, j int) bool {
			return results[i].result.tcpDuration < results[j].result.tcpDuration
		})
	}

	file, err := os.Create(*outFile)
	if err != nil {
		fmt.Printf("Unable to create file: %v\n", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	if *speedTest > 0 {
		writer.Write([]string{"IP Address", "Port", "TLS", "Data Center", "Region", "City", "Latency", "Download Speed"})
	} else {
		writer.Write([]string{"IP Address", "Port", "TLS", "Data Center", "Region", "City", "Latency"})
	}
	for _, res := range results {
		if *speedTest > 0 {
			writer.Write([]string{res.result.ip, strconv.Itoa(res.result.port), strconv.FormatBool(*enableTLS), res.result.dataCenter, res.result.region, res.result.city, res.result.latency, fmt.Sprintf("%.0f kB/s", res.downloadSpeed)})
		} else {
			writer.Write([]string{res.result.ip, strconv.Itoa(res.result.port), strconv.FormatBool(*enableTLS), res.result.dataCenter, res.result.region, res.result.city, res.result.latency})
		}
	}

	writer.Flush()
	// Clear output
	fmt.Print("\033[2J")
	fmt.Printf("Results written to file %s successfully, took %d seconds\n", *outFile, time.Since(startTime)/time.Second)
}

// Read IP addresses from file
func readIPs(file string) ([]string, error) {
	fileContent, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer fileContent.Close()
	var ips []string
	scanner := bufio.NewScanner(fileContent)
	for scanner.Scan() {
		ipAddr := scanner.Text()
		// Check if it is in CIDR format
		if strings.Contains(ipAddr, "/") {
			ip, ipNet, err := net.ParseCIDR(ipAddr)
			if err != nil {
				fmt.Printf("Unable to parse CIDR format IP: %v\n", err)
				continue
			}
			for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); inc(ip) {
				ips = append(ips, ip.String())
			}
		} else {
			ips = append(ips, ipAddr)
		}
	}
	return ips, scanner.Err()
}

// Function to increment IP address
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// Speed test function
func getDownloadSpeed(ip string) float64 {
	var protocol string
	if *enableTLS {
		protocol = "https://"
	} else {
		protocol = "http://"
	}
	speedTestURL := protocol + *speedTestURL
	// Create request
	req, _ := http.NewRequest("GET", speedTestURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	// Create TCP connection
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 0,
	}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(*defaultPort)))
	if err != nil {
		return 0
	}
	defer conn.Close()

	fmt.Printf("Testing IP %s Port %s\n", ip, strconv.Itoa(*defaultPort))
	startTime := time.Now()
	// Create HTTP client
	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
		},
		// Set maximum time for a single IP speed test to 5 seconds
		Timeout: 5 * time.Second,
	}
	// Send request
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Invalid speed test for IP %s Port %s\n", ip, strconv.Itoa(*defaultPort))
		return 0
	}
	defer resp.Body.Close()

	// Copy response body to /dev/null and calculate download speed
	written, _ := io.Copy(io.Discard, resp.Body)
	duration := time.Since(startTime)
	speed := float64(written) / duration.Seconds() / 1024

	// Output result
	fmt.Printf("IP %s Port %s Download Speed %.0f kB/s\n", ip, strconv.Itoa(*defaultPort), speed)
	return speed
}
