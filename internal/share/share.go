// Package share defines what a slice of a GPU is.
//
// A card is divided into a fixed number of seats. A request for a fraction of
// a card is rounded up to whole seats, and the job is told the share it was
// actually granted, never a rounder number. A job's compute cap and its
// default memory cap both follow from its seats.
package share

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// DefaultSeats is how many seats a card is divided into unless configured.
// Eight gives shares in steps of 12.5%, which is fine enough to be useful and
// coarse enough that MPS caps stay meaningful.
const DefaultSeats = 8

// Seats is the number of seats a fraction needs on a card with n seats.
// It rounds up, and never returns less than one or more than n.
func Seats(fraction float64, n int) (int, error) {
	if n < 1 {
		return 0, fmt.Errorf("a card needs at least one seat")
	}
	if !(fraction > 0 && fraction <= 1) {
		return 0, fmt.Errorf("share must be greater than 0 and at most 1, got %g", fraction)
	}
	s := int(math.Ceil(fraction*float64(n) - 1e-9))
	if s < 1 {
		s = 1
	}
	if s > n {
		s = n
	}
	return s, nil
}

// Granted is the fraction a number of seats actually represents.
func Granted(seats, n int) float64 { return float64(seats) / float64(n) }

// ComputePercent is the MPS active thread percentage for a number of seats.
func ComputePercent(seats, n int) int {
	p := int(math.Round(100 * float64(seats) / float64(n)))
	if p < 1 {
		p = 1
	}
	return p
}

// DefaultMemMiB is the memory a job gets when it does not ask for an amount:
// the same fraction of the card as its seats.
func DefaultMemMiB(seats, n, cardMiB int) int {
	m := int(float64(cardMiB) * float64(seats) / float64(n))
	if m < 256 {
		m = 256
	}
	return m
}

// ParseMem reads a memory amount like "20G", "512M", "1.5G" or a bare number
// of MiB, and returns MiB.
func ParseMem(s string) (int, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	if t == "" {
		return 0, fmt.Errorf("empty memory amount")
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(t, "GIB"), strings.HasSuffix(t, "GB"), strings.HasSuffix(t, "G"):
		mult = 1024
		t = strings.TrimRight(t, "GIB")
	case strings.HasSuffix(t, "MIB"), strings.HasSuffix(t, "MB"), strings.HasSuffix(t, "M"):
		t = strings.TrimRight(t, "MIB")
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("cannot read memory amount %q", s)
	}
	return int(math.Round(v * mult)), nil
}

// FormatMem renders MiB the way people write it.
func FormatMem(mib int) string {
	if mib >= 1024 && mib%1024 == 0 {
		return fmt.Sprintf("%dG", mib/1024)
	}
	if mib >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mib)/1024)
	}
	return fmt.Sprintf("%dM", mib)
}
