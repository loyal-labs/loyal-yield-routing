package webapp

// GET /api/solana-week/progress, porting
// web/apps/web/src/app/api/solana-week/progress/route.ts +
// web/apps/web/src/features/solana-week/server/quest-completion-service.ts:376-410.
// Next authenticated by a walletAddress query param; here identity is the
// Principal (bearer loyal-mobile-earn JWT or the web cookie).

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

const solanaWeekProgressPath = "/api/solana-week/progress"

// questKinds is QUEST_KINDS (quest-completion-service.ts:358): the fixed kind
// order the response always lists, started or not.
var questKinds = [2]string{"earn_deposit", "first_autodeposit_sweep"}

type SolanaWeekQuestProgressItem struct {
	Kind         string  `json:"kind"`
	Status       string  `json:"status"`
	SolanaStatus *string `json:"solanaStatus"`
	ReportedAt   *string `json:"reportedAt"`
	Attempts     int     `json:"attempts"`
}

type SolanaWeekQuestProgressResponse struct {
	WalletAddress string                        `json:"walletAddress"`
	Quests        []SolanaWeekQuestProgressItem `json:"quests"`
}

type questCompletionRow struct {
	QuestKind    string
	Status       string
	SolanaStatus *string
	ReportedAt   *string
	Attempts     int
}

type questProgressRequest struct{ SessionInput }

type questProgressOutput struct{ Body SolanaWeekQuestProgressResponse }

func (h earnRoutes) registerQuestProgress(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-solana-week-progress",
		Method:      http.MethodGet,
		Path:        solanaWeekProgressPath,
		Summary:     "Solana Week quest progress for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 401, 409, 500),
	}, h.questProgress)
}

func (h earnRoutes) questProgress(ctx context.Context, in *questProgressRequest) (*questProgressOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, questProgressFailure(err)
	}
	rows, err := h.questCompletionRows(ctx, account.Wallet.String())
	if err != nil {
		return nil, questProgressFailure(err)
	}
	return &questProgressOutput{Body: questProgressResponse(account.Wallet.String(), rows)}, nil
}

// questProgressFailure keeps auth refusals in the earn envelope and folds
// every other failure into Next's 500 internal_error body.
func questProgressFailure(err error) error {
	var refused *routeError
	if errors.As(err, &refused) {
		return refused
	}
	slog.Warn("[solana-week] quest progress lookup failed", "error", err)
	return earnError(http.StatusInternalServerError, "internal_error", "Failed to load quest progress.")
}

func (h earnRoutes) questCompletionRows(ctx context.Context, wallet string) ([]questCompletionRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := h.YieldDB.Query(ctx, `SELECT quest_kind, status, solana_status, reported_at, attempts
FROM loyal_yield.solana_week_quest_completions
WHERE wallet_address = $1`, wallet)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []questCompletionRow
	for rows.Next() {
		var reportedAt *time.Time
		row := questCompletionRow{}
		if err := rows.Scan(&row.QuestKind, &row.Status, &row.SolanaStatus, &reportedAt, &row.Attempts); err != nil {
			return nil, err
		}
		if reportedAt != nil {
			row.ReportedAt = strPtr(isoMilliseconds(*reportedAt))
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func strPtr(v string) *string { return &v }

// questProgressResponse ports getQuestProgress
// (quest-completion-service.ts:376-410): one item per known kind, rows folded
// by kind with the last row winning, unexpected statuses collapsing to
// "pending" so the UI never shows a raw value.
func questProgressResponse(wallet string, rows []questCompletionRow) SolanaWeekQuestProgressResponse {
	rowByKind := map[string]questCompletionRow{}
	for _, row := range rows {
		rowByKind[row.QuestKind] = row
	}
	quests := make([]SolanaWeekQuestProgressItem, 0, len(questKinds))
	for _, kind := range questKinds {
		row, ok := rowByKind[kind]
		if !ok {
			quests = append(quests, SolanaWeekQuestProgressItem{Kind: kind, Status: "not_started"})
			continue
		}
		quests = append(quests, SolanaWeekQuestProgressItem{
			Kind:         kind,
			Status:       questRowStatus(row.Status),
			SolanaStatus: row.SolanaStatus,
			ReportedAt:   row.ReportedAt,
			Attempts:     row.Attempts,
		})
	}
	return SolanaWeekQuestProgressResponse{WalletAddress: wallet, Quests: quests}
}

func questRowStatus(status string) string {
	if status == "reported" || status == "failed" {
		return status
	}
	return "pending"
}
