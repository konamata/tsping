package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"

	"github.com/olekukonko/tablewriter"
)

type PingResult struct {
	host       string
	externalIP string
	port       string
	pings      []string
}

func getTailscaleIPs() ([]string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "status")
	} else if runtime.GOOS == "linux" {
		cmd = exec.Command("tailscale", "status")
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "status")
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status error: %v", err)
	}

	var ips []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "offline") {
			fields := strings.Fields(line)
			if len(fields) > 0 && strings.Contains(fields[0], ".") {
				ips = append(ips, fields[1])
			}
		}
	}
	return ips, nil
}

func checkTailscale() error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "version")
	} else if runtime.GOOS == "linux" {
		cmd = exec.Command("tailscale", "version")
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "version")
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tailscale is not installed or not running: %v", err)
	}
	return nil
}

func pingIP(ip string, wg *sync.WaitGroup, results chan<- PingResult, completed *int32) {
	defer wg.Done()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "ping", "--until-direct=false", "-c", "5", ip)
	} else if runtime.GOOS == "linux" {
		cmd = exec.Command("tailscale", "ping", "--until-direct=false", "-c", "5", ip)
	} else if runtime.GOOS == "darwin" {
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "ping", "--until-direct=false", "-c", "5", ip)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		atomic.AddInt32(completed, 1)
		return
	}

	result := PingResult{host: ip}
	publicIPPattern := regexp.MustCompile(`via (\d+\.\d+\.\d+\.\d+):(\d+) in (\d+)ms`)
	pingPattern := regexp.MustCompile(`in (\d+)ms`)

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if match := publicIPPattern.FindStringSubmatch(line); match != nil {
			result.externalIP = match[1]
			result.port = match[2]
		}
		if match := pingPattern.FindStringSubmatch(line); match != nil {
			result.pings = append(result.pings, match[1])
		}
	}

	// Ensure externalIP and port are set to "N/A" if not found
	if result.externalIP == "" {
		result.externalIP = "N/A"
	}
	if result.port == "" {
		result.port = "N/A"
	}

	atomic.AddInt32(completed, 1)
	results <- result
}

func main() {
	ips, err := getTailscaleIPs()
	if err != nil {
		fmt.Printf("Error getting IPs: %v\n", err)
		return
	}

	if len(ips) == 0 {
		fmt.Println("No active Tailscale IPs found")
		return
	}

	var wg sync.WaitGroup
	var completed int32 = 0
	total := int32(len(ips))
	var resultsList []PingResult

	// Channels
	results := make(chan PingResult, len(ips))
	resultsDone := make(chan bool)
	progressDone := make(chan bool)
	timeout := time.After(10 * time.Second)

	// Progress bar setup
	bar := progressbar.NewOptions(int(total),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetWidth(15),
		progressbar.OptionSetDescription("[cyan]Pinging...[reset]"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "[green]=[reset]",
			SaucerHead:    "[green]>[reset]",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
		progressbar.OptionSetWriter(os.Stderr), // Ensures real-time output
		progressbar.OptionOnCompletion(nil),
	)

	// Start ping goroutines
	for _, ip := range ips {
		wg.Add(1)
		go pingIP(ip, &wg, results, &completed)
	}

	// Results collector
	go func() {
		for result := range results {
			resultsList = append(resultsList, result)
		}
		resultsDone <- true
	}()

	// Progress reporter
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt32(&completed)
				err := bar.Set(int(current))
				if err != nil {
					return
				}
			case <-progressDone:
				err := bar.Finish()

				fmt.Println()
				if err != nil {
					return
				}
				return
			}
		}
	}()

	// Wait for all pings to complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Wait for completion or timeout
	select {
	case <-timeout:
		fmt.Printf("\nTimeout reached. Collected %d/%d results.\n", atomic.LoadInt32(&completed), total)
	case <-resultsDone:
		fmt.Printf("\nAll results collected successfully.\n")
	}

	close(progressDone)

	// Sort by average ping time
	sort.Slice(resultsList, func(i, j int) bool {
		iAvg := calculateAverage(resultsList[i].pings)
		jAvg := calculateAverage(resultsList[j].pings)
		return iAvg < jAvg
	})

	// Table setup and render
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Host", "External IP", "Port", "Pings (ms)"})
	table.SetAutoFormatHeaders(false)

	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
	)

	table.SetColumnColor(
		tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.FgYellowColor},
		tablewriter.Colors{tablewriter.FgYellowColor},
		tablewriter.Colors{tablewriter.FgWhiteColor},
	)

	table.SetBorder(true)
	table.SetRowLine(false)
	table.SetAutoMergeCells(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	for _, result := range resultsList {
		pings := strings.Join(result.pings, ", ")
		table.Append([]string{
			result.host,
			result.externalIP,
			result.port,
			pings,
		})
	}
	table.Render()
}

func calculateAverage(pings []string) float64 {
	sum := 0
	for _, p := range pings {
		val, _ := strconv.Atoi(p)
		sum += val
	}
	return float64(sum) / float64(len(pings))
}
