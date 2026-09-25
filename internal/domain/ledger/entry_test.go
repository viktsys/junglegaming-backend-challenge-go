package ledger

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func newIDs(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	walletID, err := uuid.NewV7()
	require.NoError(t, err)
	transactionID, err := uuid.NewV7()
	require.NoError(t, err)
	return walletID, transactionID
}

func TestNewCreditEntry(t *testing.T) {
	walletID, transactionID := newIDs(t)
	entry, err := New(walletID, transactionID, DirectionCredit,
		money.MustParse("25.00", "BRL"),
		money.MustParse("100.00", "BRL"),
		money.MustParse("125.00", "BRL"),
		time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, DirectionCredit, entry.Direction())
	assert.Equal(t, "125.00", entry.BalanceAfter().AmountString())
}

func TestNewDebitEntry(t *testing.T) {
	walletID, transactionID := newIDs(t)
	entry, err := New(walletID, transactionID, DirectionDebit,
		money.MustParse("80.00", "BRL"),
		money.MustParse("100.00", "BRL"),
		money.MustParse("20.00", "BRL"),
		time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, DirectionDebit, entry.Direction())
}

func TestNewRejectsMathMismatch(t *testing.T) {
	walletID, transactionID := newIDs(t)
	_, err := New(walletID, transactionID, DirectionCredit,
		money.MustParse("25.00", "BRL"),
		money.MustParse("100.00", "BRL"),
		money.MustParse("120.00", "BRL"),
		time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, CodeMathMismatch, apperr.CodeOf(err))

	_, err = New(walletID, transactionID, DirectionDebit,
		money.MustParse("25.00", "BRL"),
		money.MustParse("100.00", "BRL"),
		money.MustParse("80.00", "BRL"),
		time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, CodeMathMismatch, apperr.CodeOf(err))
}

func TestNewRejectsInvalidInputs(t *testing.T) {
	walletID, transactionID := newIDs(t)
	now := time.Now().UTC()

	_, err := New(uuid.Nil, transactionID, DirectionCredit,
		money.MustParse("1.00", "BRL"), money.MustParse("0.00", "BRL"), money.MustParse("1.00", "BRL"), now)
	require.Error(t, err)

	_, err = New(walletID, transactionID, Direction("SIDEWAYS"),
		money.MustParse("1.00", "BRL"), money.MustParse("0.00", "BRL"), money.MustParse("1.00", "BRL"), now)
	require.Error(t, err)

	_, err = New(walletID, transactionID, DirectionCredit,
		money.MustParse("0.00", "BRL"), money.MustParse("0.00", "BRL"), money.MustParse("0.00", "BRL"), now)
	require.Error(t, err)

	_, err = New(walletID, transactionID, DirectionDebit,
		money.MustParse("1.00", "BRL"), money.MustParse("0.00", "BRL"), money.MustParse("-1.00", "BRL"), now)
	require.Error(t, err)

	_, err = New(walletID, transactionID, DirectionCredit,
		money.MustParse("1.00", "BRL"), money.MustParse("0.00", "BRL"), money.MustParse("1.00", "BRL"), time.Time{})
	require.Error(t, err)
}

func TestRehydrateValidatesInvariants(t *testing.T) {
	walletID, transactionID := newIDs(t)
	entryID, err := uuid.NewV7()
	require.NoError(t, err)

	entry, err := Rehydrate(entryID, walletID, transactionID, DirectionCredit,
		money.MustParse("10.00", "BRL"),
		money.MustParse("0.00", "BRL"),
		money.MustParse("10.00", "BRL"),
		time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, entryID, entry.ID())
}
