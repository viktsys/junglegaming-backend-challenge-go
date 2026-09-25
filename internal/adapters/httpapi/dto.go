// Package httpapi exposes the HTTP contract of the service.
package httpapi

import (
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain/money"
)

type openWalletRequest struct {
	PlayerID       string      `json:"playerId"`
	InitialBalance money.Money `json:"initialBalance"`
}

type walletResponse struct {
	ID       string      `json:"id"`
	PlayerID string      `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

type ledgerEntryResponse struct {
	ID            string      `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Money         money.Money `json:"money"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

type submitTransactionRequest struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
}

type submitTransactionResponse struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          *money.Money `json:"balance,omitempty"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

type transactionResponse struct {
	TransactionID                  string       `json:"transactionId"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Kind                           string       `json:"kind"`
	Status                         string       `json:"status"`
	Money                          money.Money  `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	FailureCorrectable             *bool        `json:"failureCorrectable,omitempty"`
	Balance                        *money.Money `json:"balance,omitempty"`
	Attempts                       int          `json:"attempts"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
}

type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
}

type errorResponse struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	Kind             string `json:"kind"`
	FailureCode      string `json:"failureCode,omitempty"`
	TransactionID    string `json:"transactionId,omitempty"`
	IdempotentReplay bool   `json:"idempotentReplay,omitempty"`
}
