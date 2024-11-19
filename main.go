package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/olekukonko/tablewriter"
	"github.com/schollz/progressbar/v3"
)

type PingResult struct {
	ip         string
	hostname   string
	user       string
	os         string
	externalIP string
	port       string
	pings      []string
	group      int
}

func getTailscaleStatus() ([]PingResult, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "status")
	case "linux":
		cmd = exec.Command("tailscale", "status")
	case "darwin":
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "status")
	default:
		return nil, fmt.Errorf("unsupported OS")
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status error: %v", err)
	}

	var results []PingResult
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, "Self") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		status := strings.Join(fields[4:], " ")
		if strings.Contains(status, "offline") {
			continue
		}

		result := PingResult{
			ip:       fields[0],
			hostname: fields[1],
			user:     fields[2],
			os:       fields[3],
		}

		results = append(results, result)
	}
	return results, nil
}

func checkTailscale() error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "version")
	case "linux":
		cmd = exec.Command("tailscale", "version")
	case "darwin":
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "version")
	default:
		return fmt.Errorf("unsupported OS")
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tailscale is not installed or not running: %v", err)
	}
	return nil
}

func pingIP(result *PingResult, wg *sync.WaitGroup, completed *int32) {
	defer wg.Done() // Only one Done() call here

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "ping", "--until-direct=false", "-c", "3", result.ip)
	case "linux":
		cmd = exec.Command("tailscale", "ping", "--until-direct=false", "-c", "3", result.ip)
	case "darwin":
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "ping", "--until-direct=false", "-c", "3", result.ip)
	default:
		atomic.AddInt32(completed, 1)
		return
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		atomic.AddInt32(completed, 1)
		return
	}

	publicIPPattern := regexp.MustCompile(`via (\d+\.\d+\.\d+\.\d+):(\d+)`)
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

	atomic.AddInt32(completed, 1)
}

func isPublicIP(ip string) bool {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	// Define private IP ranges
	privateBlocks := []string{
		"10.0.0.0/8",
		"127.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // Link-local addresses
		"100.64.0.0/10",  // Carrier-grade NAT
	}

	for _, cidr := range privateBlocks {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(parsedIP) {
			return false
		}
	}

	// If not private, it's public
	return true
}

func calculateAverage(pings []string) float64 {
	sum := 0
	for _, p := range pings {
		val, err := strconv.Atoi(p)
		if err == nil {
			sum += val
		}
	}
	if len(pings) == 0 {
		return 0
	}
	return float64(sum) / float64(len(pings))
}

func main() {
	err := checkTailscale()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	resultsList, err := getTailscaleStatus()
	if err != nil {
		fmt.Printf("Error getting Tailscale status: %v\n", err)
		return
	}

	if len(resultsList) == 0 {
		fmt.Println("No active Tailscale IPs found")
		return
	}

	var wg sync.WaitGroup
	var completed int32
	total := int32(len(resultsList))

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
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprintf(os.Stderr, "\n") // Add newline after progress bar completion
		}),
	)

	// Start ping goroutines
	for i := range resultsList {
		wg.Add(1)
		go func(result *PingResult) {
			pingIP(result, &wg, &completed)
			bar.Add(1)
		}(&resultsList[i])
	}

	wg.Wait()

	// Assign group numbers based on external IP if external IP is not empty and is public
	groupMap := make(map[string]int)
	groupCounter := 1

	for i := range resultsList {
		externalIP := resultsList[i].externalIP
		if externalIP == "" || !isPublicIP(externalIP) {
			continue // Skip if external IP is empty or not public
		}
		if _, exists := groupMap[externalIP]; !exists {
			groupMap[externalIP] = groupCounter
			groupCounter++
		}
		resultsList[i].group = groupMap[externalIP]
	}

	// Sort the resultsList
	sort.Slice(resultsList, func(i, j int) bool {
		if resultsList[i].group != resultsList[j].group {
			return resultsList[i].group < resultsList[j].group
		}
		iAvg := calculateAverage(resultsList[i].pings)
		jAvg := calculateAverage(resultsList[j].pings)
		return iAvg < jAvg
	})

	// Table setup and render
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"#", "Group", "User", "Hostname", "OS", "Tailscale IP", "External IP", "Port", "Pings (ms)"})
	table.SetAutoFormatHeaders(false)

	// Add this line to set column alignments
	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_LEFT,   // #
		tablewriter.ALIGN_CENTER, // Group
		tablewriter.ALIGN_LEFT,   // User
		tablewriter.ALIGN_LEFT,   // Hostname
		tablewriter.ALIGN_CENTER, // OS
		tablewriter.ALIGN_LEFT,   // Tailscale IP
		tablewriter.ALIGN_LEFT,   // External IP
		tablewriter.ALIGN_CENTER, // Port
		tablewriter.ALIGN_CENTER, // Pings
	})

	table.SetHeaderColor(
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
	)

	table.SetColumnColor(
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgMagentaColor},
		tablewriter.Colors{tablewriter.FgGreenColor},
		tablewriter.Colors{tablewriter.FgYellowColor},
		tablewriter.Colors{tablewriter.FgHiRedColor},
		tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.FgBlueColor},
		tablewriter.Colors{tablewriter.FgBlueColor},
		tablewriter.Colors{tablewriter.FgWhiteColor},
	)

	table.SetBorder(true)
	table.SetRowLine(false)
	table.SetAutoMergeCells(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	i := 1
	for _, result := range resultsList {
		pings := strings.Join(result.pings, " ")
		groupStr := ""
		if result.group > 0 {
			groupStr = strconv.Itoa(result.group)
		}
		table.Append([]string{
			strconv.Itoa(i),
			groupStr,
			result.user,
			result.hostname,
			result.os,
			result.ip,
			result.externalIP,
			result.port,
			pings,
		})

		i++
	}
	table.Render()
}
