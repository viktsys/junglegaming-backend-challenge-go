package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/junglegaming/backend-challenge-go/internal/app/wallets"
	"github.com/junglegaming/backend-challenge-go/internal/domain/apperr"
	"github.com/junglegaming/backend-challenge-go/internal/domain/wallet"
)

// WalletHandler serves wallet lifecycle and read endpoints.
type WalletHandler struct {
	service *wallets.Service
}

// NewWalletHandler builds the handler.
func NewWalletHandler(service *wallets.Service) *WalletHandler {
	return &WalletHandler{service: service}
}

// Open handles POST /wallets.
func (h *WalletHandler) Open(w http.ResponseWriter, r *http.Request) {
	var request openWalletRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	playerID, err := uuid.Parse(request.PlayerID)
	if err != nil {
		writeError(w, apperr.Wrap(apperr.KindInvalid, "INVALID_PLAYER_ID", "playerId must be a UUID", err))
		return
	}

	opened, err := h.service.Open(r.Context(), wallets.OpenCommand{
		PlayerID:       playerID,
		InitialBalance: request.InitialBalance,
		CorrelationID:  RequestIDFrom(r.Context()),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toWalletResponse(opened))
}

// Get handles GET /wallets/{walletId}.
func (h *WalletHandler) Get(w http.ResponseWriter, r *http.Request) {
	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		writeError(w, err)
		return
	}
	found, err := h.service.Get(r.Context(), walletID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toWalletResponse(found))
}

// Ledger handles GET /wallets/{walletId}/ledger.
func (h *WalletHandler) Ledger(w http.ResponseWriter, r *http.Request) {
	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"), 50)
	if err != nil {
		writeError(w, err)
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, err)
		return
	}

	page, err := h.service.Ledger(r.Context(), walletID, cursor, limit)
	if err != nil {
		writeError(w, err)
		return
	}

	response := ledgerResponse{Entries: make([]ledgerEntryResponse, 0, len(page.Entries))}
	for _, entry := range page.Entries {
		response.Entries = append(response.Entries, ledgerEntryResponse{
			ID:            entry.ID().String(),
			TransactionID: entry.TransactionID().String(),
			Direction:     string(entry.Direction()),
			Money:         entry.Amount(),
			BalanceBefore: entry.BalanceBefore(),
			BalanceAfter:  entry.BalanceAfter(),
			CreatedAt:     entry.CreatedAt(),
		})
	}
	if page.HasMore && len(page.Entries) > 0 {
		last := page.Entries[len(page.Entries)-1]
		response.NextCursor = encodeCursor(last.CreatedAt(), last.ID())
	}
	writeJSON(w, http.StatusOK, response)
}

// Reconcile handles POST /wallets/{walletId}/reconciliation.
func (h *WalletHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	walletID, err := pathUUID(r, "walletId")
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := h.service.Reconcile(r.Context(), walletID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID:          result.WalletID.String(),
		StoredBalance:     result.StoredBalance,
		CalculatedBalance: result.CalculatedBalance,
		Difference:        result.Difference,
		Consistent:        result.Consistent,
		CheckedEntries:    result.CheckedEntries,
	})
}

func toWalletResponse(w *wallet.Wallet) walletResponse {
	return walletResponse{
		ID:       w.ID().String(),
		PlayerID: w.PlayerID().String(),
		Balance:  w.Balance(),
		Version:  w.Version(),
	}
}

func pathUUID(r *http.Request, parameter string) (uuid.UUID, error) {
	raw := chi.URLParam(r, parameter)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperr.Wrap(apperr.KindInvalid, "INVALID_IDENTIFIER",
			parameter+" must be a UUID", err)
	}
	return id, nil
}
