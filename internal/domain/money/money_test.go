package money

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
)

func TestParseValid(t *testing.T) {
	cases := []struct {
		amount   string
		currency string
		expected int64
	}{
		{"0", "BRL", 0},
		{"0.00", "BRL", 0},
		{"0.5", "BRL", 50},
		{"25", "BRL", 2500},
		{"25.00", "BRL", 2500},
		{"25.5", "BRL", 2550},
		{"-3.14", "BRL", -314},
		{"00025.00", "BRL", 2500},
		{"92233720368547758.07", "BRL", math.MaxInt64},
		{"-92233720368547758.07", "BRL", math.MinInt64 + 1},
		{"-92233720368547758.08", "BRL", math.MinInt64},
		{"-0.00", "BRL", 0},
		{"1.00", "USD", 100},
	}
	for _, tc := range cases {
		t.Run(tc.amount+tc.currency, func(t *testing.T) {
			parsed, err := Parse(tc.amount, tc.currency)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, parsed.MinorUnits())
			assert.Equal(t, Currency(tc.currency), parsed.Currency())
		})
	}
}

func TestParseRejectsInvalidInputs(t *testing.T) {
	invalidAmounts := []string{
		"", " ", "abc", "1e2", "1E2", "NaN", "nan", "Infinity", "-Infinity",
		"1.234", ".5", "1.", "+1.00", "1,00", "0x10", " 25.00", "25.00 ",
		"1 0", "--1", "1..2", "1.2.3", "92233720368547758.08",
		"-92233720368547758.09", "99999999999999999999", "0.000", "١٢٣",
	}
	for _, amount := range invalidAmounts {
		t.Run(amount, func(t *testing.T) {
			_, err := Parse(amount, "BRL")
			require.Error(t, err)
			assert.Equal(t, apperr.KindInvalid, apperr.KindOf(err))
		})
	}

	for _, currency := range []string{"", "brl", "BR", "BRLL", "12A", "R$"} {
		t.Run("currency_"+currency, func(t *testing.T) {
			_, err := Parse("10.00", currency)
			require.Error(t, err)
			assert.Equal(t, CodeInvalidCurrency, apperr.CodeOf(err))
		})
	}
}

func TestParseNonNegativeAndPositive(t *testing.T) {
	_, err := ParseNonNegative("-0.01", "BRL")
	require.Error(t, err)
	assert.Equal(t, CodeNegativeAmount, apperr.CodeOf(err))

	parsed, err := ParseNonNegative("0.00", "BRL")
	require.NoError(t, err)
	assert.True(t, parsed.IsZero())

	_, err = ParsePositive("0.00", "BRL")
	require.Error(t, err)
	assert.Equal(t, CodeZeroAmount, apperr.CodeOf(err))

	parsed, err = ParsePositive("0.01", "BRL")
	require.NoError(t, err)
	assert.Equal(t, int64(1), parsed.MinorUnits())
}

func TestZeroValueIsInvalid(t *testing.T) {
	var zero Money
	assert.False(t, zero.IsValid())
	_, err := zero.Add(MustParse("1.00", "BRL"))
	require.Error(t, err)
	assert.Equal(t, CodeInvalidCurrency, apperr.CodeOf(err))
	_, err = zero.MarshalJSON()
	require.Error(t, err)
}

func TestArithmetic(t *testing.T) {
	a := MustParse("100.00", "BRL")
	b := MustParse("25.50", "BRL")

	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.Equal(t, "125.50", sum.AmountString())

	diff, err := a.Sub(b)
	require.NoError(t, err)
	assert.Equal(t, "74.50", diff.AmountString())

	neg, err := diff.Neg()
	require.NoError(t, err)
	assert.Equal(t, "-74.50", neg.AmountString())

	absolute, err := neg.Abs()
	require.NoError(t, err)
	assert.Equal(t, "74.50", absolute.AmountString())
}

func TestArithmeticRejectsCurrencyMismatch(t *testing.T) {
	brl := MustParse("10.00", "BRL")
	usd := MustParse("10.00", "USD")

	_, err := brl.Add(usd)
	require.Error(t, err)
	assert.Equal(t, CodeCurrencyMismatch, apperr.CodeOf(err))

	_, err = brl.Sub(usd)
	require.Error(t, err)
	assert.Equal(t, CodeCurrencyMismatch, apperr.CodeOf(err))

	_, err = brl.Compare(usd)
	require.Error(t, err)
	assert.Equal(t, CodeCurrencyMismatch, apperr.CodeOf(err))
}

func TestArithmeticOverflow(t *testing.T) {
	max := MustFromMinorUnits(math.MaxInt64, "BRL")
	min := MustFromMinorUnits(math.MinInt64, "BRL")
	cent := MustParse("0.01", "BRL")

	_, err := max.Add(cent)
	require.Error(t, err)
	assert.Equal(t, CodeOverflow, apperr.CodeOf(err))

	_, err = min.Sub(cent)
	require.Error(t, err)
	assert.Equal(t, CodeOverflow, apperr.CodeOf(err))

	_, err = min.Neg()
	require.Error(t, err)
	assert.Equal(t, CodeOverflow, apperr.CodeOf(err))
}

func TestCompare(t *testing.T) {
	ten := MustParse("10.00", "BRL")
	eleven := MustParse("11.00", "BRL")

	cmp, err := ten.Compare(eleven)
	require.NoError(t, err)
	assert.Equal(t, -1, cmp)

	less, err := ten.LessThan(eleven)
	require.NoError(t, err)
	assert.True(t, less)

	greater, err := eleven.GreaterThan(ten)
	require.NoError(t, err)
	assert.True(t, greater)

	equal, err := ten.Equal(MustParse("10.00", "BRL"))
	require.NoError(t, err)
	assert.True(t, equal)
}

func TestFormatMinorUnits(t *testing.T) {
	assert.Equal(t, "0.00", FormatMinorUnits(0))
	assert.Equal(t, "0.05", FormatMinorUnits(5))
	assert.Equal(t, "1.00", FormatMinorUnits(100))
	assert.Equal(t, "-1.05", FormatMinorUnits(-105))
	assert.Equal(t, "92233720368547758.07", FormatMinorUnits(math.MaxInt64))
	assert.Equal(t, "-92233720368547758.08", FormatMinorUnits(math.MinInt64))
}

func TestJSONRoundTrip(t *testing.T) {
	original := MustParse("25.00", "BRL")
	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount":"25.00","currency":"BRL"}`, string(encoded))

	var decoded Money
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	equal, err := original.Equal(decoded)
	require.NoError(t, err)
	assert.True(t, equal)
}

func TestJSONRejectsNonCanonicalAmounts(t *testing.T) {
	cases := []string{
		`{"amount":25.00,"currency":"BRL"}`,
		`{"amount":"25.001","currency":"BRL"}`,
		`{"amount":"NaN","currency":"BRL"}`,
		`{"amount":"1e2","currency":"BRL"}`,
		`{"amount":"","currency":"BRL"}`,
		`{"amount":"25.00"}`,
		`{"currency":"BRL"}`,
		`"25.00"`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			var decoded Money
			require.Error(t, json.Unmarshal([]byte(raw), &decoded))
		})
	}
}

func TestFromMinorUnitsRejectsEmptyCurrency(t *testing.T) {
	_, err := FromMinorUnits(100, "")
	require.Error(t, err)
	assert.Equal(t, CodeInvalidCurrency, apperr.CodeOf(err))
}
