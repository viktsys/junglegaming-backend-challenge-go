package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/app/wager"
	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wagering"
)

// WageringHandler serves wager transaction endpoints.
type WageringHandler struct {
	service *wager.Service
}

// NewWageringHandler builds the handler.
func NewWageringHandler(service *wager.Service) *WageringHandler {
	return &WageringHandler{service: service}
}

// Submit handles POST /wagering/transactions.
func (h *WageringHandler) Submit(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, apperr.New(apperr.KindInvalid, "MISSING_IDEMPOTENCY_KEY",
			"the Idempotency-Key header is required"))
		return
	}

	var request submitTransactionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if !authorizeProvider(w, r, request.ProviderID) {
		return
	}

	playerID, err := uuid.Parse(request.PlayerID)
	if err != nil {
		writeError(w, apperr.Wrap(apperr.KindInvalid, "INVALID_PLAYER_ID", "playerId must be a UUID", err))
		return
	}
	walletID, err := uuid.Parse(request.WalletID)
	if err != nil {
		writeError(w, apperr.Wrap(apperr.KindInvalid, "INVALID_WALLET_ID", "walletId must be a UUID", err))
		return
	}
	kind, err := wagering.ParseKind(request.Kind)
	if err != nil {
		writeError(w, err)
		return
	}

	result, err := h.service.Process(r.Context(), wager.ProcessCommand{
		ProviderID:                     request.ProviderID,
		ExternalTransactionID:          request.ExternalTransactionID,
		IdempotencyKey:                 idempotencyKey,
		PlayerID:                       playerID,
		WalletID:                       walletID,
		RoundID:                        request.RoundID,
		GameID:                         request.GameID,
		Kind:                           kind,
		Money:                          request.Money,
		ReferenceExternalTransactionID: request.ReferenceExternalTransactionID,
		CorrelationID:                  RequestIDFrom(r.Context()),
	})
	if err != nil {
		writeError(w, err)
		return
	}

	response := toSubmitResponse(result)
	switch result.Status {
	case wagering.StatusRejected:
		writeRejection(w, response.TransactionID, result.FailureCode,
			"operation rejected by a business rule", result.IdempotentReplay)
	case wagering.StatusPending, wagering.StatusPendingReference:
		writeJSON(w, http.StatusAccepted, response)
	default:
		if result.IdempotentReplay {
			writeJSON(w, http.StatusOK, response)
			return
		}
		writeJSON(w, http.StatusCreated, response)
	}
}

// Get handles GET /wagering/transactions/{transactionId}.
func (h *WageringHandler) Get(w http.ResponseWriter, r *http.Request) {
	transactionID, err := pathUUID(r, "transactionId")
	if err != nil {
		writeError(w, err)
		return
	}
	transaction, err := h.service.GetTransaction(r.Context(), transactionID)
	if err != nil {
		writeError(w, err)
		return
	}
	if !authorizeProvider(w, r, transaction.ProviderID()) {
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(transaction))
}

// GetByExternal handles GET /providers/{providerId}/wagering/transactions/{externalTransactionId}.
func (h *WageringHandler) GetByExternal(w http.ResponseWriter, r *http.Request) {
	providerID := chi.URLParam(r, "providerId")
	if !authorizeProvider(w, r, providerID) {
		return
	}
	externalID := chi.URLParam(r, "externalTransactionId")
	transaction, err := h.service.GetByProviderExternal(r.Context(), providerID, externalID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTransactionResponse(transaction))
}

func toSubmitResponse(result wager.Result) submitTransactionResponse {
	response := submitTransactionResponse{
		TransactionID:    result.Transaction.ID().String(),
		Status:           string(result.Status),
		FailureCode:      result.FailureCode,
		IdempotentReplay: result.IdempotentReplay,
	}
	if result.HasBalance {
		balance := result.Balance
		response.Balance = &balance
	}
	return response
}

func toTransactionResponse(t *wagering.Transaction) transactionResponse {
	response := transactionResponse{
		TransactionID:                  t.ID().String(),
		ProviderID:                     t.ProviderID(),
		ExternalTransactionID:          t.ExternalTransactionID(),
		PlayerID:                       t.PlayerID().String(),
		WalletID:                       t.WalletID().String(),
		RoundID:                        t.RoundID(),
		GameID:                         t.GameID(),
		Kind:                           string(t.Kind()),
		Status:                         string(t.Status()),
		Money:                          t.Money(),
		ReferenceExternalTransactionID: t.ReferenceExternalTransactionID(),
		FailureCode:                    t.FailureCode(),
		Attempts:                       t.Attempts(),
		CreatedAt:                      t.CreatedAt(),
		UpdatedAt:                      t.UpdatedAt(),
	}
	if balance, ok := t.ResultBalance(); ok {
		response.Balance = &balance
	}
	if t.FailureCode() != "" {
		correctable := wagering.IsCorrectableFailure(t.FailureCode())
		response.FailureCorrectable = &correctable
	}
	return response
}
