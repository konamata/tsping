package main

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	"sync/atomic"
	"time"

	"github.com/olekukonko/tablewriter"
	"github.com/schollz/progressbar/v3"
)

// PingResult
type PingResult struct {
	ip         string
	hostname   string
	user       string
	os         string
	externalIP string
	port       string
	pings      []string
	group      string
	isp        string
}

// IPInfo represents the JSON structure returned by ip-api.com
type IPInfo struct {
	Status    string  `json:"status"`
	ISP       string  `json:"isp"`
	Country   string  `json:"country"`
	Region    string  `json:"regionName"`
	City      string  `json:"city"`
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	Query     string  `json:"query"`
}

// getIPInfo fonksiyonu ekleyelim
func getIPInfo(ip string) (string, error) {
	if ip == "" {
		return "", nil
	}

	// Rate limiting için kısa bir bekleme
	time.Sleep(100 * time.Millisecond)

	url := fmt.Sprintf("http://ip-api.com/json/%s", ip)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var info IPInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}

	return info.ISP, nil
}

// Helper function to convert number to letter group with count
func numberToLetterWithCount(n int, count int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%s (%d)", string(rune('A'+(n-1))), count)
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
	defer wg.Done()

	// Ping işlemi
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("C:\\Program Files\\Tailscale\\tailscale.exe", "ping", "--until-direct=false", "-c", "5", result.ip)
	case "linux":
		cmd = exec.Command("tailscale", "ping", "--until-direct=false", "-c", "5", result.ip)
	case "darwin":
		cmd = exec.Command("/Applications/Tailscale.app/Contents/MacOS/tailscale", "ping", "--until-direct=false", "-c", "5", result.ip)
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

			// ISP bilgisini al
			if isp, err := getIPInfo(result.externalIP); err == nil {
				result.isp = isp
			}
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

	privateBlocks := []string{
		"10.0.0.0/8",
		"127.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
	}

	for _, cidr := range privateBlocks {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(parsedIP) {
			return false
		}
	}

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

	bar := progressbar.NewOptions(len(resultsList),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionSetWidth(15),
		progressbar.OptionSetDescription("[cyan]Getting ISP info...[reset]"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "[green]=[reset]",
			SaucerHead:    "[green]>[reset]",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprintf(os.Stderr, "\n")
		}),
	)

	for i := range resultsList {
		wg.Add(1)
		go func(result *PingResult) {
			pingIP(result, &wg, &completed)
			bar.Add(1)
		}(&resultsList[i])
	}

	wg.Wait()

	// Use maps to track group numbers and counts
	groupMap := make(map[string]int)
	groupCounter := 1
	groupCounts := make(map[string]int)

	// First pass: Count devices per external IP
	for _, result := range resultsList {
		if result.externalIP != "" && isPublicIP(result.externalIP) {
			groupCounts[result.externalIP]++
		}
	}

	// Second pass: Assign groups with counts
	for i := range resultsList {
		externalIP := resultsList[i].externalIP
		if externalIP == "" || !isPublicIP(externalIP) {
			continue
		}
		if _, exists := groupMap[externalIP]; !exists {
			groupMap[externalIP] = groupCounter
			groupCounter++
		}
		count := groupCounts[externalIP]
		resultsList[i].group = numberToLetterWithCount(groupMap[externalIP], count)
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

	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"#", "User", "Hostname", "OS", "Tailscale IP", "Group", "External IP", "Port", "Ping", "ISP"})
	table.SetAutoFormatHeaders(false)

	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_RIGHT,
		tablewriter.ALIGN_LEFT,
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
		tablewriter.Colors{tablewriter.FgHiGreenColor},
	)

	table.SetColumnColor(
		tablewriter.Colors{tablewriter.FgHiGreenColor},
		tablewriter.Colors{tablewriter.FgGreenColor},
		tablewriter.Colors{tablewriter.FgYellowColor},
		tablewriter.Colors{tablewriter.FgHiRedColor},
		tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.FgMagentaColor},
		tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.FgCyanColor},
		tablewriter.Colors{tablewriter.FgWhiteColor},
		tablewriter.Colors{tablewriter.FgHiGreenColor},
	)

	table.SetBorder(true)
	table.SetRowLine(false)
	table.SetAutoMergeCells(false)
	table.SetAlignment(tablewriter.ALIGN_LEFT)

	i := 1
	for _, result := range resultsList {
		avgPing := calculateAverage(result.pings)
		avgPingStr := "-"
		if avgPing > 0 {
			avgPingStr = fmt.Sprintf("%.1f", avgPing)
		}

		table.Append([]string{
			strconv.Itoa(i),
			result.user,
			result.hostname,
			result.os,
			result.ip,
			result.group,
			result.externalIP,
			result.port,
			avgPingStr,
			result.isp,
		})
		i++
	}
	table.Render()
}
