package wagering

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

func ids(t *testing.T) (uuid.UUID, uuid.UUID) {
	t.Helper()
	walletID, err := uuid.NewV7()
	require.NoError(t, err)
	playerID, err := uuid.NewV7()
	require.NoError(t, err)
	return walletID, playerID
}

func baseParams(t *testing.T, kind Kind, amount string) ExternalParams {
	t.Helper()
	walletID, playerID := ids(t)
	parsed, err := money.Parse(amount, "BRL")
	require.NoError(t, err)
	return ExternalParams{
		ProviderID:            "provider-a",
		ExternalTransactionID: "tx-1",
		IdempotencyKey:        "provider-a:tx-1",
		PayloadHash:           "hash",
		WalletID:              walletID,
		PlayerID:              playerID,
		RoundID:               "round-1",
		GameID:                "fortune-chimp",
		Kind:                  kind,
		Money:                 parsed,
	}
}

func TestNewExternalAcceptsEachKind(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		kind   Kind
		amount string
	}{
		{KindBet, "25.00"},
		{KindWin, "25.00"},
		{KindLoss, "0.00"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			params := baseParams(t, tc.kind, tc.amount)
			tx, err := NewExternal(params, now)
			require.NoError(t, err)
			assert.Equal(t, StatusPending, tx.Status())
			assert.Equal(t, OriginExternal, tx.Origin())
			assert.Equal(t, tc.kind, tx.Kind())
		})
	}
}

func TestNewExternalRejectsOpening(t *testing.T) {
	params := baseParams(t, KindOpening, "10.00")
	_, err := NewExternal(params, time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, CodeInvalidKind, apperr.CodeOf(err))
}

func TestNewExternalAmountRules(t *testing.T) {
	now := time.Now().UTC()

	_, err := NewExternal(baseParams(t, KindBet, "0.00"), now)
	require.Error(t, err)
	assert.Equal(t, FailureInvalidAmount, apperr.CodeOf(err))

	_, err = NewExternal(baseParams(t, KindWin, "0.00"), now)
	require.Error(t, err)

	_, err = NewExternal(baseParams(t, KindRefund, "0.00"), now)
	require.Error(t, err)

	params := baseParams(t, KindLoss, "0.01")
	_, err = NewExternal(params, now)
	require.Error(t, err)
	assert.Equal(t, FailureInvalidAmount, apperr.CodeOf(err))
}

func TestNewExternalRejectsNegativeAmount(t *testing.T) {
	params := baseParams(t, KindBet, "-10.00")
	_, err := NewExternal(params, time.Now().UTC())
	require.Error(t, err)
	assert.Equal(t, money.CodeNegativeAmount, apperr.CodeOf(err))
}

func TestNewExternalReferenceRules(t *testing.T) {
	now := time.Now().UTC()

	for _, kind := range []Kind{KindRefund, KindRollback} {
		params := baseParams(t, kind, "10.00")
		_, err := NewExternal(params, now)
		require.Error(t, err, string(kind))

		params.ReferenceExternalTransactionID = "tx-original"
		_, err = NewExternal(params, now)
		require.NoError(t, err, string(kind))
	}

	bet := baseParams(t, KindBet, "10.00")
	bet.ReferenceExternalTransactionID = "tx-original"
	_, err := NewExternal(bet, now)
	require.Error(t, err)

	win := baseParams(t, KindWin, "10.00")
	win.ReferenceExternalTransactionID = "tx-original"
	_, err = NewExternal(win, now)
	require.NoError(t, err)
}

func TestNewExternalRequiredFields(t *testing.T) {
	now := time.Now().UTC()
	params := baseParams(t, KindBet, "10.00")

	mutations := map[string]func(*ExternalParams){
		"provider": func(p *ExternalParams) { p.ProviderID = "" },
		"external": func(p *ExternalParams) { p.ExternalTransactionID = "" },
		"key":      func(p *ExternalParams) { p.IdempotencyKey = "" },
		"hash":     func(p *ExternalParams) { p.PayloadHash = "" },
		"wallet":   func(p *ExternalParams) { p.WalletID = uuid.Nil },
		"player":   func(p *ExternalParams) { p.PlayerID = uuid.Nil },
		"round":    func(p *ExternalParams) { p.RoundID = "" },
		"game":     func(p *ExternalParams) { p.GameID = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := params
			mutate(&candidate)
			_, err := NewExternal(candidate, now)
			require.Error(t, err)
		})
	}

	_, err := NewExternal(params, time.Time{})
	require.Error(t, err)
}

func TestNewOpening(t *testing.T) {
	walletID, playerID := ids(t)
	now := time.Now().UTC()
	opening, err := NewOpening(OpeningParams{
		WalletID: walletID,
		PlayerID: playerID,
		Money:    money.MustParse("1000.00", "BRL"),
	}, now)
	require.NoError(t, err)
	assert.Equal(t, KindOpening, opening.Kind())
	assert.Equal(t, OriginInternal, opening.Origin())
	assert.Equal(t, StatusProcessed, opening.Status())
	assert.Equal(t, "", opening.ProviderID())

	_, err = NewOpening(OpeningParams{
		WalletID: walletID,
		PlayerID: playerID,
		Money:    money.MustParse("0.00", "BRL"),
	}, now)
	require.Error(t, err)
}

func TestStateTransitions(t *testing.T) {
	now := time.Now().UTC()
	params := baseParams(t, KindRefund, "10.00")
	params.ReferenceExternalTransactionID = "tx-original"
	tx, err := NewExternal(params, now)
	require.NoError(t, err)

	require.NoError(t, tx.MarkPendingReference(now))
	assert.Equal(t, StatusPendingReference, tx.Status())
	require.NoError(t, tx.ScheduleRetry(now.Add(time.Second), now))
	assert.Equal(t, 1, tx.Attempts())

	require.NoError(t, tx.MarkProcessed(money.MustParse("10.00", "BRL"), now))
	assert.Equal(t, StatusProcessed, tx.Status())
	assert.True(t, tx.IsTerminal())

	balance, ok := tx.ResultBalance()
	require.True(t, ok)
	assert.Equal(t, "10.00", balance.AmountString())

	// Terminal states reject further transitions.
	require.Error(t, tx.MarkRejected(FailureReversalConflict, nil, now))
	require.Error(t, tx.MarkFailed("boom", now))
	require.Error(t, tx.MarkPendingReference(now))
	require.Error(t, tx.ScheduleRetry(now, now))
}

func TestMarkRejectedRequiresCode(t *testing.T) {
	tx, err := NewExternal(baseParams(t, KindBet, "10.00"), time.Now().UTC())
	require.NoError(t, err)
	require.Error(t, tx.MarkRejected("", nil, time.Now().UTC()))

	require.NoError(t, tx.MarkRejected(FailureInsufficientFunds, nil, time.Now().UTC()))
	assert.Equal(t, FailureInsufficientFunds, tx.FailureCode())
	assert.True(t, tx.IsTerminal())
}

func TestMarkFailed(t *testing.T) {
	tx, err := NewExternal(baseParams(t, KindBet, "10.00"), time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, tx.MarkFailed("PERMANENT_INFRA", time.Now().UTC()))
	assert.Equal(t, StatusFailed, tx.Status())
}

func TestScheduleRetryFromPending(t *testing.T) {
	tx, err := NewExternal(baseParams(t, KindBet, "10.00"), time.Now().UTC())
	require.NoError(t, err)

	next := time.Now().UTC().Add(2 * time.Second)
	require.NoError(t, tx.ScheduleRetry(next, time.Now().UTC()))
	assert.Equal(t, 1, tx.Attempts())
	stored, ok := tx.NextAttemptAt()
	require.True(t, ok)
	assert.WithinDuration(t, next, stored, time.Millisecond)

	require.Error(t, tx.ScheduleRetry(time.Time{}, time.Now().UTC()))
}

func TestRehydrateValidation(t *testing.T) {
	walletID, playerID := ids(t)
	amount := money.MustParse("10.00", "BRL")
	now := time.Now().UTC()
	balance := amount

	base := RehydrateParams{
		ID:                    uuid.Must(uuid.NewV7()),
		Origin:                OriginExternal,
		ProviderID:            "provider-a",
		ExternalTransactionID: "tx-1",
		IdempotencyKey:        "provider-a:tx-1",
		PayloadHash:           "hash",
		WalletID:              walletID,
		PlayerID:              playerID,
		RoundID:               "round-1",
		GameID:                "game-1",
		Kind:                  KindBet,
		Money:                 amount,
		Status:                StatusProcessed,
		ResultBalance:         &balance,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	_, err := Rehydrate(base)
	require.NoError(t, err)

	missingBalance := base
	missingBalance.ResultBalance = nil
	_, err = Rehydrate(missingBalance)
	require.Error(t, err)

	lossWithoutBalance := base
	lossWithoutBalance.Kind = KindLoss
	lossWithoutBalance.Money = money.MustParse("0.00", "BRL")
	lossWithoutBalance.ResultBalance = nil
	_, err = Rehydrate(lossWithoutBalance)
	require.NoError(t, err)

	rejectedWithoutCode := base
	rejectedWithoutCode.Status = StatusRejected
	rejectedWithoutCode.ResultBalance = nil
	_, err = Rehydrate(rejectedWithoutCode)
	require.Error(t, err)
}

func TestIsCorrectableFailure(t *testing.T) {
	assert.True(t, IsCorrectableFailure(FailureInsufficientFunds))
	assert.True(t, IsCorrectableFailure(FailureReferenceNotFound))
	assert.False(t, IsCorrectableFailure(FailureReversalConflict))
	assert.False(t, IsCorrectableFailure(FailureReferenceMismatch))
}

func TestParseKindAndStatus(t *testing.T) {
	kind, err := ParseKind("BET")
	require.NoError(t, err)
	assert.Equal(t, KindBet, kind)

	_, err = ParseKind("bet")
	require.Error(t, err)

	_, err = ParseKind("OPENING_EXTRA")
	require.Error(t, err)

	status, err := ParseStatus("PENDING_REFERENCE")
	require.NoError(t, err)
	assert.Equal(t, StatusPendingReference, status)

	_, err = ParseStatus("UNKNOWN")
	require.Error(t, err)
}

func TestStatusTerminal(t *testing.T) {
	assert.False(t, StatusPending.IsTerminal())
	assert.False(t, StatusPendingReference.IsTerminal())
	assert.True(t, StatusProcessed.IsTerminal())
	assert.True(t, StatusRejected.IsTerminal())
	assert.True(t, StatusFailed.IsTerminal())
}
