package main

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/olekukonko/tablewriter"
	"os"
)

type PingResult struct {
	host     string
	publicIP string
	port     string
	pings    []string
}

func getTailscaleIPs() ([]string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "status")
	} else {
		cmd = exec.Command("tailscale", "status")
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
			if len(fields) > 1 && strings.Contains(fields[1], ".") {
				ips = append(ips, fields[1])
			}
		}
	}
	return ips, nil
}

func pingIP(ip string, wg *sync.WaitGroup, results chan<- PingResult) {
	defer wg.Done()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "ping", "--until-direct=false", "-c", "5", ip)
	} else {
		cmd = exec.Command("tailscale", "ping", "--until-direct=false", "-c", "5", ip)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return
	}

	result := PingResult{host: ip}
	publicIPPattern := regexp.MustCompile(`via (\d+\.\d+\.\d+\.\d+):(\d+) in (\d+)ms`)
	pingPattern := regexp.MustCompile(`in (\d+)ms`)

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if match := publicIPPattern.FindStringSubmatch(line); match != nil {
			result.publicIP = match[1]
			result.port = match[2]
		}
		if match := pingPattern.FindStringSubmatch(line); match != nil {
			result.pings = append(result.pings, match[1])
		}
	}

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
	results := make(chan PingResult, len(ips))

	for _, ip := range ips {
		wg.Add(1)
		go pingIP(ip, &wg, results)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Host", "Public IP", "Port", "Pings (ms)"})

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

	for result := range results {
		pings := strings.Join(result.pings, ", ")
		table.Append([]string{
			result.host,
			result.publicIP,
			result.port,
			pings,
		})
	}

	table.Render()
}
