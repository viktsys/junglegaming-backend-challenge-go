package wallet

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func newPlayer(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	return id
}

func TestOpen(t *testing.T) {
	player := newPlayer(t)
	now := time.Now().UTC()

	opened, err := Open(player, money.MustParse("1000.00", "BRL"), now)
	require.NoError(t, err)
	assert.Equal(t, player, opened.PlayerID())
	assert.Equal(t, money.Currency("BRL"), opened.Currency())
	assert.Equal(t, "1000.00", opened.Balance().AmountString())
	assert.Equal(t, int64(1), opened.Version())
	assert.Equal(t, now, opened.CreatedAt())
	assert.Equal(t, now, opened.UpdatedAt())
}

func TestOpenWithZeroBalance(t *testing.T) {
	opened, err := Open(newPlayer(t), money.MustParse("0.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, opened.Balance().IsZero())
	assert.Equal(t, int64(1), opened.Version())
}

func TestOpenRejectsInvalidInput(t *testing.T) {
	now := time.Now().UTC()
	valid := money.MustParse("10.00", "BRL")

	_, err := Open(uuid.Nil, valid, now)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidWallet, apperr.CodeOf(err))

	_, err = Open(newPlayer(t), money.Money{}, now)
	require.Error(t, err)

	_, err = Open(newPlayer(t), money.MustParse("-1.00", "BRL"), now)
	require.Error(t, err)
	assert.Equal(t, CodeInvalidWallet, apperr.CodeOf(err))

	_, err = Open(newPlayer(t), valid, time.Time{})
	require.Error(t, err)
}

func TestDebitAndCreditChangeBalanceAndVersion(t *testing.T) {
	opened, err := Open(newPlayer(t), money.MustParse("100.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)

	now := time.Now().UTC().Add(time.Second)
	change, err := opened.Debit(money.MustParse("80.00", "BRL"), now)
	require.NoError(t, err)
	assert.Equal(t, "100.00", change.Before.AmountString())
	assert.Equal(t, "20.00", change.After.AmountString())
	assert.Equal(t, int64(2), opened.Version())
	assert.Equal(t, now, opened.UpdatedAt())

	change, err = opened.Credit(money.MustParse("5.50", "BRL"), now.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, "20.00", change.Before.AmountString())
	assert.Equal(t, "25.50", change.After.AmountString())
	assert.Equal(t, int64(3), opened.Version())
}

func TestDebitRejectsInsufficientFunds(t *testing.T) {
	opened, err := Open(newPlayer(t), money.MustParse("100.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)

	_, err = opened.Debit(money.MustParse("100.01", "BRL"), time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, CodeInsufficientFunds, apperr.CodeOf(err))
	assert.Equal(t, apperr.KindRejected, apperr.KindOf(err))
	assert.Equal(t, "100.00", opened.Balance().AmountString())
	assert.Equal(t, int64(1), opened.Version())
}

func TestDebitAllowsExactBalance(t *testing.T) {
	opened, err := Open(newPlayer(t), money.MustParse("100.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)

	change, err := opened.Debit(money.MustParse("100.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, change.After.IsZero())
}

func TestMovementRejectsCurrencyMismatchAndNonPositive(t *testing.T) {
	opened, err := Open(newPlayer(t), money.MustParse("100.00", "BRL"), time.Now().UTC())
	require.NoError(t, err)

	_, err = opened.Credit(money.MustParse("10.00", "USD"), time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, CodeCurrencyMismatch, apperr.CodeOf(err))

	_, err = opened.Credit(money.MustParse("0.00", "BRL"), time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, CodeNonPositiveAmount, apperr.CodeOf(err))

	_, err = opened.Debit(money.MustParse("-1.00", "BRL"), time.Now().UTC())
	require.Error(t, err)

	_, err = opened.Credit(money.MustParse("1.00", "BRL"), time.Time{})
	require.Error(t, err)
}

func TestRehydrate(t *testing.T) {
	id, err := uuid.NewV7()
	require.NoError(t, err)
	player := newPlayer(t)
	createdAt := time.Now().UTC().Add(-time.Hour)
	updatedAt := time.Now().UTC()

	rehydrated, err := Rehydrate(id, player, "BRL", 12345, 7, createdAt, updatedAt)
	require.NoError(t, err)
	assert.Equal(t, "123.45", rehydrated.Balance().AmountString())
	assert.Equal(t, int64(7), rehydrated.Version())

	_, err = Rehydrate(uuid.Nil, player, "BRL", 0, 1, createdAt, updatedAt)
	require.Error(t, err)

	_, err = Rehydrate(id, player, "BRL", -1, 1, createdAt, updatedAt)
	require.Error(t, err)

	_, err = Rehydrate(id, player, "BRL", 0, 0, createdAt, updatedAt)
	require.Error(t, err)

	_, err = Rehydrate(id, player, "BRL", 0, 1, updatedAt, createdAt)
	require.Error(t, err)
}
