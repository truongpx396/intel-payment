package paypal

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// exponent is how many decimal places PayPal's amounts have in a currency. PayPal takes whole units only in these three,
// and two places in every other it supports.
func exponent(currency string) int {
	switch strings.ToUpper(currency) {
	case "HUF", "JPY", "TWD":
		return 0
	}
	return 2
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ParseAmount reads a PayPal decimal string — "10.00", "1000" for a yen — as integer minor units, without a float. It
// takes digits, an optional point and at most as many decimals as the currency has; anything else (a sign, an exponent,
// a separator, a space, another script's digits) and anything past int64 is an error.
func ParseAmount(value, currency string) (int64, error) {
	if currency == "" {
		return 0, fmt.Errorf("%w: an amount %q is in no currency", md.ErrInvalid, value)
	}
	exp := exponent(currency)
	whole, frac, hasFrac := strings.Cut(value, ".")
	if !isDigits(whole) || (hasFrac && (!isDigits(frac) || len(frac) > exp)) {
		return 0, fmt.Errorf("%w: %q is not an amount in %s (digits, and at most %d after the point)", md.ErrInvalid, value, currency, exp)
	}
	var n uint64
	for i := range len(whole) {
		d := uint64(whole[i] - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, fmt.Errorf("%w: %q is past what an amount can be", md.ErrInvalid, value)
		}
		n = n*10 + d
	}
	var scale uint64 = 1
	for range exp {
		scale *= 10
	}
	hi, lo := bits.Mul64(n, scale)
	if hi != 0 {
		return 0, fmt.Errorf("%w: %q is past what an amount can be", md.ErrInvalid, value)
	}
	var f uint64
	if hasFrac {
		f, _ = strconv.ParseUint(frac+strings.Repeat("0", exp-len(frac)), 10, 64) // digits only, and at most exp of them
	}
	sum, carry := bits.Add64(lo, f, 0)
	if carry != 0 || sum > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %q is past what an amount can be", md.ErrInvalid, value)
	}
	return int64(sum), nil
}

// FormatAmount writes integer minor units as PayPal's decimal string: "10.00", or "1500" for a yen.
func FormatAmount(minor int64, currency string) (string, error) {
	if currency == "" {
		return "", fmt.Errorf("%w: an amount of %d is in no currency", md.ErrInvalid, minor)
	}
	if minor < 0 {
		return "", fmt.Errorf("%w: an amount of %d is negative", md.ErrInvalid, minor)
	}
	exp := exponent(currency)
	s := strconv.FormatInt(minor, 10)
	if exp == 0 {
		return s, nil
	}
	if len(s) <= exp {
		s = strings.Repeat("0", exp-len(s)+1) + s
	}
	return s[:len(s)-exp] + "." + s[len(s)-exp:], nil
}
