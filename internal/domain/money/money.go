// Package money implements an immutable Money value object backed by int64
// minor units (for BRL: centavos) and an ISO 4217 currency code.
//
// Representation: amount is stored as int64 in the currency's minor unit.
// BRL therefore supports values in the range [-92233720368547758.08,
// 92233720368547758.07]. No floating point type ever participates in parsing,
// arithmetic, serialization or persistence.
package money

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
)

// Scale is the fixed number of decimal places accepted for all currencies.
const Scale = 2

// MinorUnitsPerMajor is 10^Scale.
const MinorUnitsPerMajor int64 = 100

const (
	// CodeInvalidAmount is returned for unparseable or out-of-scale amounts.
	CodeInvalidAmount = "INVALID_MONEY_AMOUNT"
	// CodeInvalidCurrency is returned for malformed ISO 4217 codes.
	CodeInvalidCurrency = "INVALID_MONEY_CURRENCY"
	// CodeCurrencyMismatch is returned when mixing currencies in an operation.
	CodeCurrencyMismatch = "MONEY_CURRENCY_MISMATCH"
	// CodeOverflow is returned when an arithmetic operation overflows int64.
	CodeOverflow = "MONEY_OVERFLOW"
	// CodeNegativeAmount is returned when a non-negative amount is required.
	CodeNegativeAmount = "NEGATIVE_MONEY_AMOUNT"
	// CodeZeroAmount is returned when a strictly positive amount is required.
	CodeZeroAmount = "ZERO_MONEY_AMOUNT"
)

var (
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	// amountPattern accepts plain decimal notation with at most two decimal
	// places. It intentionally rejects scientific notation, NaN, Infinity,
	// leading '+', empty fractions and thousands separators.
	amountPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]{1,2})?$`)
)

// Currency is an ISO 4217 alphabetic code (three uppercase letters).
type Currency string

// NewCurrency validates and returns a currency code.
func NewCurrency(code string) (Currency, error) {
	if !currencyPattern.MatchString(code) {
		return "", apperr.New(apperr.KindInvalid, CodeInvalidCurrency,
			fmt.Sprintf("currency %q is not a valid ISO 4217 alphabetic code", code))
	}
	return Currency(code), nil
}

// String returns the ISO code.
func (c Currency) String() string { return string(c) }

// IsZero reports whether the currency is unset.
func (c Currency) IsZero() bool { return c == "" }

// Money is an immutable amount in a currency. The zero value is invalid.
type Money struct {
	amount   int64
	currency Currency
}

// Parse parses a decimal string into Money. Negative values are accepted
// because internal calculations (differences, reversals) require them; use
// ParseNonNegative or ParsePositive for external financial inputs.
func Parse(amount, currency string) (Money, error) {
	cur, err := NewCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	units, err := parseMinorUnits(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{amount: units, currency: cur}, nil
}

// ParseNonNegative parses a decimal string and rejects negative values.
func ParseNonNegative(amount, currency string) (Money, error) {
	m, err := Parse(amount, currency)
	if err != nil {
		return Money{}, err
	}
	if m.IsNegative() {
		return Money{}, apperr.New(apperr.KindInvalid, CodeNegativeAmount,
			"amount must not be negative")
	}
	return m, nil
}

// ParsePositive parses a decimal string and requires a strictly positive value.
func ParsePositive(amount, currency string) (Money, error) {
	m, err := Parse(amount, currency)
	if err != nil {
		return Money{}, err
	}
	if !m.IsPositive() {
		return Money{}, apperr.New(apperr.KindInvalid, CodeZeroAmount,
			"amount must be greater than zero")
	}
	return m, nil
}

// FromMinorUnits builds Money from an exact amount in minor units.
func FromMinorUnits(units int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, apperr.New(apperr.KindInvalid, CodeInvalidCurrency, "currency is required")
	}
	if _, err := NewCurrency(currency.String()); err != nil {
		return Money{}, err
	}
	return Money{amount: units, currency: currency}, nil
}

// MustFromMinorUnits is FromMinorUnits for constants and tests; it panics on invalid input.
func MustFromMinorUnits(units int64, currency Currency) Money {
	m, err := FromMinorUnits(units, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// MustParse is Parse for constants and tests; it panics on invalid input.
func MustParse(amount, currency string) Money {
	m, err := Parse(amount, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero returns the zero amount for a currency.
func Zero(currency Currency) (Money, error) {
	return FromMinorUnits(0, currency)
}

// IsValid reports whether the value was built through a constructor.
func (m Money) IsValid() bool { return !m.currency.IsZero() }

// MinorUnits returns the exact amount in minor units.
func (m Money) MinorUnits() int64 { return m.amount }

// Currency returns the currency of the amount.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.amount == 0 }

// IsPositive reports whether the amount is greater than zero.
func (m Money) IsPositive() bool { return m.amount > 0 }

// IsNegative reports whether the amount is less than zero.
func (m Money) IsNegative() bool { return m.amount < 0 }

// Add returns m + other. Both operands must share a currency and the result
// must not overflow int64.
func (m Money) Add(other Money) (Money, error) {
	if err := m.assertCompatible(other); err != nil {
		return Money{}, err
	}
	sum := m.amount + other.amount
	if (other.amount > 0 && sum < m.amount) || (other.amount < 0 && sum > m.amount) {
		return Money{}, apperr.New(apperr.KindInvalid, CodeOverflow, "money addition overflowed int64")
	}
	return Money{amount: sum, currency: m.currency}, nil
}

// Sub returns m - other. Both operands must share a currency and the result
// must not overflow int64.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.assertCompatible(other); err != nil {
		return Money{}, err
	}
	diff := m.amount - other.amount
	if (other.amount < 0 && diff < m.amount) || (other.amount > 0 && diff > m.amount) {
		return Money{}, apperr.New(apperr.KindInvalid, CodeOverflow, "money subtraction overflowed int64")
	}
	return Money{amount: diff, currency: m.currency}, nil
}

// Neg returns -m, rejecting the single unrepresentable value.
func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, apperr.New(apperr.KindInvalid, CodeOverflow, "money negation overflowed int64")
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// Abs returns the absolute value of m.
func (m Money) Abs() (Money, error) {
	if m.amount < 0 {
		return m.Neg()
	}
	return m, nil
}

// Compare returns -1, 0 or 1 comparing m to other. Currencies must match.
func (m Money) Compare(other Money) (int, error) {
	if err := m.assertCompatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.amount < other.amount:
		return -1, nil
	case m.amount > other.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether both amounts are equal and in the same currency.
func (m Money) Equal(other Money) (bool, error) {
	cmp, err := m.Compare(other)
	if err != nil {
		return false, err
	}
	return cmp == 0, nil
}

// LessThan reports whether m < other.
func (m Money) LessThan(other Money) (bool, error) {
	cmp, err := m.Compare(other)
	if err != nil {
		return false, err
	}
	return cmp < 0, nil
}

// GreaterThan reports whether m > other.
func (m Money) GreaterThan(other Money) (bool, error) {
	cmp, err := m.Compare(other)
	if err != nil {
		return false, err
	}
	return cmp > 0, nil
}

// AmountString returns the canonical fixed-scale decimal string ("25.00").
func (m Money) AmountString() string { return FormatMinorUnits(m.amount) }

// String returns the canonical fixed-scale decimal string.
func (m Money) String() string { return m.AmountString() }

// MarshalJSON encodes the value as {"amount":"25.00","currency":"BRL"}.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, fmt.Errorf("money: cannot marshal an uninitialized value")
	}
	type wire struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	return json.Marshal(wire{Amount: m.AmountString(), Currency: m.currency.String()})
}

// UnmarshalJSON decodes {"amount":"25.00","currency":"BRL"} strictly.
// JSON numbers are rejected because the amount field is a string.
func (m *Money) UnmarshalJSON(data []byte) error {
	var wire struct {
		Amount   *string `json:"amount"`
		Currency *string `json:"currency"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return apperr.Wrap(apperr.KindInvalid, CodeInvalidAmount, "invalid money object", err)
	}
	if wire.Amount == nil || wire.Currency == nil {
		return apperr.New(apperr.KindInvalid, CodeInvalidAmount, "money requires amount and currency")
	}
	parsed, err := Parse(*wire.Amount, *wire.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

func (m Money) assertCompatible(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return apperr.New(apperr.KindInvalid, CodeInvalidCurrency, "money operands must be initialized")
	}
	if m.currency != other.currency {
		return apperr.New(apperr.KindInvalid, CodeCurrencyMismatch,
			fmt.Sprintf("cannot combine %s with %s", m.currency, other.currency))
	}
	return nil
}

// FormatMinorUnits renders minor units as a fixed-scale decimal string.
func FormatMinorUnits(units int64) string {
	negative := units < 0
	// Work with uint64 to handle math.MinInt64 safely.
	var absolute uint64
	if negative {
		absolute = uint64(-(units + 1)) + 1
	} else {
		absolute = uint64(units)
	}
	major := absolute / uint64(MinorUnitsPerMajor)
	minor := absolute % uint64(MinorUnitsPerMajor)
	formatted := strconv.FormatUint(major, 10) + "." + fmt.Sprintf("%02d", minor)
	if negative {
		return "-" + formatted
	}
	return formatted
}

// parseMinorUnits parses a strict decimal string into int64 minor units.
func parseMinorUnits(amount string) (int64, error) {
	if amount == "" {
		return 0, apperr.New(apperr.KindInvalid, CodeInvalidAmount, "amount is required")
	}
	if !amountPattern.MatchString(amount) {
		return 0, apperr.New(apperr.KindInvalid, CodeInvalidAmount,
			fmt.Sprintf("amount %q must be a decimal with at most %d places", amount, Scale))
	}

	negative := strings.HasPrefix(amount, "-")
	unsigned := strings.TrimPrefix(amount, "-")

	majorPart := unsigned
	minorPart := "0"
	if idx := strings.IndexByte(unsigned, '.'); idx >= 0 {
		majorPart = unsigned[:idx]
		minorPart = unsigned[idx+1:]
		for len(minorPart) < Scale {
			minorPart += "0"
		}
	}

	major, err := strconv.ParseUint(majorPart, 10, 64)
	if err != nil {
		return 0, apperr.New(apperr.KindInvalid, CodeOverflow, "amount overflows the supported range")
	}
	minor, err := strconv.ParseUint(minorPart, 10, 64)
	if err != nil {
		return 0, apperr.New(apperr.KindInvalid, CodeInvalidAmount, "invalid fractional part")
	}

	maxMagnitude := uint64(math.MaxInt64)
	if negative {
		// The magnitude of math.MinInt64 is MaxInt64 + 1, so the full
		// symmetric range is representable.
		maxMagnitude++
	}
	if major > (maxMagnitude-minor)/uint64(MinorUnitsPerMajor) {
		return 0, apperr.New(apperr.KindInvalid, CodeOverflow, "amount overflows the supported range")
	}
	magnitude := major*uint64(MinorUnitsPerMajor) + minor

	if !negative {
		return int64(magnitude), nil
	}
	if magnitude == uint64(math.MaxInt64)+1 {
		return math.MinInt64, nil
	}
	return -int64(magnitude), nil
}
