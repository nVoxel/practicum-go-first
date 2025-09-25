package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultURL      = "http://srv.msk01.gigacorp.local/_stats"
	defaultInterval = 5 * time.Second

	loadAvgThreshold        = 30
	memUsageThreshold       = 0.8
	diskUsageThreshold      = 0.9
	netBandwidthThreshold   = 0.9
	bytesPerMiB             = 1024 * 1024
	bitsPerMegabitPerSecond = 1_000_000
	maxConsecutiveErrors    = 3
	httpTimeout             = 5 * time.Second
)

type stats struct {
	LoadAvg        float64
	MemTotalBytes  int64
	MemUsedBytes   int64
	DiskTotalBytes int64
	DiskUsedBytes  int64
	NetCapacityBps int64
	NetUsageBps    int64
}

func main() {
	url := getenvDurationOrString("STATS_URL", defaultURL)
	interval := getenvDuration("POLL_INTERVAL", defaultInterval)

	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   3 * time.Second,
			ResponseHeaderTimeout: 3 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
		},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	consecutiveErrors := 0

	// Немедленно сделать первый опрос, затем по тикеру
	if err := pollOnce(ctx, client, url); err != nil {
		consecutiveErrors++
		if consecutiveErrors >= maxConsecutiveErrors {
			fmt.Println("Unable to fetch server statistic")
		}
	} else {
		consecutiveErrors = 0
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pollOnce(ctx, client, url); err != nil {
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					fmt.Println("Unable to fetch server statistic")
				}
			} else {
				consecutiveErrors = 0
			}
		}
	}
}

func pollOnce(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	body, err := readAllTrim(resp.Body)
	if err != nil {
		return err
	}

	st, err := parseStats(body)
	if err != nil {
		return err
	}

	checkThresholds(st)
	return nil
}

func checkThresholds(s stats) {
	// 1) Load Average
	if s.LoadAvg > loadAvgThreshold {
		fmt.Printf("Load Average is too high: %s\n", formatFloat(s.LoadAvg))
	}

	// 2) Memory usage
	if s.MemTotalBytes <= 0 {
		return
	}
	memUsage := float64(s.MemUsedBytes) / float64(s.MemTotalBytes)
	if memUsage > memUsageThreshold {
		percent := int64(memUsage * 100)
		fmt.Printf("Memory usage too high: %d%%\n", percent)
	}

	// 3) Disk usage
	if s.DiskTotalBytes <= 0 {
		return
	}
	diskUsage := float64(s.DiskUsedBytes) / float64(s.DiskTotalBytes)
	if diskUsage > diskUsageThreshold {
		freeBytes := s.DiskTotalBytes - s.DiskUsedBytes
		if freeBytes < 0 {
			freeBytes = 0
		}
		freeMiB := freeBytes / bytesPerMiB
		fmt.Printf("Free disk space is too low: %d Mb left\n", freeMiB)
	}

	// 4) Network bandwidth usage
	if s.NetCapacityBps <= 0 {
		return
	}
	netUsage := float64(s.NetUsageBps) / float64(s.NetCapacityBps)
	if netUsage > netBandwidthThreshold {
		freeBps := s.NetCapacityBps - s.NetUsageBps
		if freeBps < 0 {
			freeBps = 0
		}
		freeMBps := freeBps / 1_000_000
		fmt.Printf("Network bandwidth usage high: %d Mbit/s available\n", freeMBps)
	}
}

func parseStats(s string) (stats, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 7 {
		return stats{}, errors.New("invalid stats format: expected 7 comma-separated numbers")
	}

	trim := func(i int) string { return strings.TrimSpace(parts[i]) }

	load, err := strconv.ParseFloat(trim(0), 64)
	if err != nil {
		return stats{}, fmt.Errorf("parse load average: %w", err)
	}

	memTotal, err := parseInt64(trim(1))
	if err != nil {
		return stats{}, fmt.Errorf("parse mem total: %w", err)
	}
	memUsed, err := parseInt64(trim(2))
	if err != nil {
		return stats{}, fmt.Errorf("parse mem used: %w", err)
	}

	diskTotal, err := parseInt64(trim(3))
	if err != nil {
		return stats{}, fmt.Errorf("parse disk total: %w", err)
	}
	diskUsed, err := parseInt64(trim(4))
	if err != nil {
		return stats{}, fmt.Errorf("parse disk used: %w", err)
	}

	netCap, err := parseInt64(trim(5))
	if err != nil {
		return stats{}, fmt.Errorf("parse net capacity: %w", err)
	}
	netUsed, err := parseInt64(trim(6))
	if err != nil {
		return stats{}, fmt.Errorf("parse net usage: %w", err)
	}

	return stats{
		LoadAvg:        load,
		MemTotalBytes:  memTotal,
		MemUsedBytes:   memUsed,
		DiskTotalBytes: diskTotal,
		DiskUsedBytes:  diskUsed,
		NetCapacityBps: netCap,
		NetUsageBps:    netUsed,
	}, nil
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func readAllTrim(r io.Reader) (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(r)
	const maxCap = 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, maxCap)
	for sc.Scan() {
		b.WriteString(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

func roundToNearest(v float64) float64 {
	if v >= 0 {
		return float64(int64(v + 0.5))
	}
	return float64(int64(v - 0.5))
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func getenvDurationOrString(env string, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

func getenvDuration(env string, def time.Duration) time.Duration {
	if v := os.Getenv(env); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
