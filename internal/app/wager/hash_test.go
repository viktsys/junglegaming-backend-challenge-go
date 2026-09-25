package wager

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
)

func hashCommand(t *testing.T) ProcessCommand {
	t.Helper()
	playerID, err := uuid.NewV7()
	require.NoError(t, err)
	walletID, err := uuid.NewV7()
	require.NoError(t, err)
	return ProcessCommand{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  wagering.KindBet,
		Money:                 money.MustParse("25.00", "BRL"),
		CorrelationID:         "corr-1",
	}
}

func TestComputePayloadHashIsDeterministic(t *testing.T) {
	cmd := hashCommand(t)
	first := ComputePayloadHash(cmd)
	second := ComputePayloadHash(cmd)
	assert.Equal(t, first, second)
	assert.Len(t, first, 64)
}

func TestComputePayloadHashExcludesIdempotencyKeyAndTransport(t *testing.T) {
	cmd := hashCommand(t)
	base := ComputePayloadHash(cmd)

	cmd.IdempotencyKey = "another-key"
	cmd.CorrelationID = "another-correlation"
	cmd.CausationID = "another-causation"
	assert.Equal(t, base, ComputePayloadHash(cmd))
}

func TestComputePayloadHashNormalizesAmounts(t *testing.T) {
	cmd := hashCommand(t)
	cmd.Money = money.MustParse("25.00", "BRL")
	first := ComputePayloadHash(cmd)
	cmd.Money = money.MustParse("25", "BRL")
	second := ComputePayloadHash(cmd)
	assert.Equal(t, first, second)
}

func TestComputePayloadHashChangesWithBusinessFields(t *testing.T) {
	base := hashCommand(t)
	baseHash := ComputePayloadHash(base)

	mutations := map[string]func(*ProcessCommand){
		"provider": func(c *ProcessCommand) { c.ProviderID = "provider-b" },
		"external": func(c *ProcessCommand) { c.ExternalTransactionID = "transaction-456" },
		"player":   func(c *ProcessCommand) { c.PlayerID = uuid.New() },
		"wallet":   func(c *ProcessCommand) { c.WalletID = uuid.New() },
		"round":    func(c *ProcessCommand) { c.RoundID = "round-999" },
		"game":     func(c *ProcessCommand) { c.GameID = "other-game" },
		"kind":     func(c *ProcessCommand) { c.Kind = wagering.KindWin },
		"amount":   func(c *ProcessCommand) { c.Money = money.MustParse("25.01", "BRL") },
		"currency": func(c *ProcessCommand) { c.Money = money.MustParse("25.00", "USD") },
		"reference": func(c *ProcessCommand) {
			c.ReferenceExternalTransactionID = "transaction-000"
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			assert.NotEqual(t, baseHash, ComputePayloadHash(candidate))
		})
	}
}

func TestComputePayloadHashHTTPAndSQSShareFields(t *testing.T) {
	cmd := hashCommand(t)
	httpHash := ComputePayloadHash(cmd)

	// The SQS path builds the same command from the message data.
	sqsCommand := ProcessCommand{
		ProviderID:            cmd.ProviderID,
		ExternalTransactionID: cmd.ExternalTransactionID,
		IdempotencyKey:        cmd.IdempotencyKey,
		PlayerID:              cmd.PlayerID,
		WalletID:              cmd.WalletID,
		RoundID:               cmd.RoundID,
		GameID:                cmd.GameID,
		Kind:                  cmd.Kind,
		Money:                 cmd.Money,
		CorrelationID:         "msg-123",
	}
	assert.Equal(t, httpHash, ComputePayloadHash(sqsCommand))
}
