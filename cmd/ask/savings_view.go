package main

import (
	"fmt"
	"strconv"
	"strings"
)

// percent renders saved/raw as an integer percentage, or "-" when raw is
// unknown (older ledger entries recorded no raw total).
func percent(saved, raw int) string {
	if raw <= 0 {
		return "-"
	}
	return strconv.Itoa(saved*100/raw) + "%"
}

// formatTokenCount humanizes a token count: 512, 1.4k, 21.8M.
func formatTokenCount(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimDotZero(fmt.Sprintf("%.1f", float64(n)/1000)) + "k"
	default:
		return trimDotZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "M"
	}
}

func trimDotZero(s string) string { return strings.TrimSuffix(s, ".0") }
