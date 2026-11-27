package domain

import (
	"fmt"
	"regexp"
)

// Money is an amount in integer minor units of an ISO 4217 currency
// (LLD §5.1). Money is never a float anywhere in Billing.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// minorUnitExponent lists ISO 4217 currencies whose minor-unit exponent is
// not 2. Every other well-formed code uses 2 (USD cents, AED fils, §8.5.3).
var minorUnitExponent = map[string]int{
	// exponent 0
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0,
	"PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	// exponent 3
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	// exponent 4
	"CLF": 4, "UYW": 4,
}

// ValidCurrency reports whether code is a well-formed ISO 4217 code.
func ValidCurrency(code string) bool { return currencyCode.MatchString(code) }

// MinorUnitExponent returns the ISO 4217 minor-unit exponent of code.
func MinorUnitExponent(code string) (int, error) {
	if !ValidCurrency(code) {
		return 0, fmt.Errorf("invalid currency code %q", code)
	}
	if e, ok := minorUnitExponent[code]; ok {
		return e, nil
	}
	return 2, nil
}

// NewMoney validates the currency and returns the value.
func NewMoney(amountMinor int64, currency string) (Money, error) {
	if !ValidCurrency(currency) {
		return Money{}, fmt.Errorf("invalid currency code %q", currency)
	}
	return Money{AmountMinor: amountMinor, Currency: currency}, nil
}

// Add returns m + o. Different currencies are an error, never a conversion.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("currency mismatch: %s and %s", m.Currency, o.Currency)
	}
	return Money{AmountMinor: m.AmountMinor + o.AmountMinor, Currency: m.Currency}, nil
}

// MulInt returns m × n (for example unit price × seats).
func (m Money) MulInt(n int64) Money {
	return Money{AmountMinor: m.AmountMinor * n, Currency: m.Currency}
}

// IsZero reports whether the amount is zero.
func (m Money) IsZero() bool { return m.AmountMinor == 0 }
