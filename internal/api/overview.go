package api

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/rcss-labs/yamdrake/internal/analytics"
	"github.com/rcss-labs/yamdrake/internal/auth"
)

const defaultOverviewDays = 14

func (s *server) overviewRoutes() []routeDef {
	return []routeDef{
		{"GET /api/v1/analytics/overview", auth.View, s.getOverview},
	}
}

type overviewResponse struct {
	From                      time.Time             `json:"from"`
	To                        time.Time             `json:"to"`
	Runs                      int                   `json:"runs"`
	PrevRuns                  int                   `json:"prev_runs"`
	SuccessRate               *float64              `json:"success_rate"`
	PrevSuccessRate           *float64              `json:"prev_success_rate"`
	MedianDurationSeconds     *int64                `json:"median_duration_seconds"`
	PrevMedianDurationSeconds *int64                `json:"prev_median_duration_seconds"`
	WaitingApproval           int                   `json:"waiting_approval"`
	OldestWaiting             *time.Time            `json:"oldest_waiting"`
	Days                      []overviewDay         `json:"days"`
	TopFailing                []overviewFailure     `json:"top_failing"`
	Attention                 []overviewAttention   `json:"attention"`
	Environments              []overviewEnvironment `json:"environments"`
}

type overviewDay struct {
	Date      string `json:"date"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Other     int    `json:"other"`
}

type overviewFailure struct {
	Service      string `json:"service"`
	Failures     int    `json:"failures"`
	CommonReason string `json:"common_reason"`
}

type overviewAttention struct {
	RunID       int64     `json:"run_id"`
	State       string    `json:"state"`
	Job         string    `json:"job"`
	Environment string    `json:"environment"`
	Label       string    `json:"label"`
	Reason      string    `json:"reason"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
}

type overviewEnvironment struct {
	Environment string     `json:"environment"`
	Label       string     `json:"label"`
	RunID       *int64     `json:"run_id"`
	State       string     `json:"state"`
	Job         string     `json:"job"`
	At          *time.Time `json:"at"`
}

func roundedSeconds(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	seconds := int64(math.Round(d.Seconds()))
	return &seconds
}

// getOverview answers GET /api/v1/analytics/overview?label=&days=.
func (s *server) getOverview(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	days := defaultOverviewDays
	if query.Has("days") {
		n, err := strconv.Atoi(query.Get("days"))
		if err != nil || (n != 7 && n != 14 && n != 30) {
			invalid(w, "days must be 7, 14 or 30")
			return
		}
		days = n
	}
	label := query.Get("label")
	if label != "" {
		_, inventory, _, active, err := s.inventoryView(r.Context())
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		known := false
		if active {
			for _, environment := range inventory.Environments {
				known = known || environment.Label == label
			}
		}
		if !known {
			invalid(w, "unknown label")
			return
		}
	}
	overview, err := analytics.Load(r.Context(), s.Pool, principal, label, days, s.Now())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, shapeOverview(overview))
}

func shapeOverview(o analytics.Overview) overviewResponse {
	out := overviewResponse{
		From: o.From, To: o.To, Runs: o.Runs, PrevRuns: o.PrevRuns,
		SuccessRate: o.SuccessRate, PrevSuccessRate: o.PrevSuccessRate,
		MedianDurationSeconds: roundedSeconds(o.MedianDuration), PrevMedianDurationSeconds: roundedSeconds(o.PrevMedianDuration),
		WaitingApproval: o.WaitingApproval, OldestWaiting: o.OldestWaiting,
		Days: []overviewDay{}, TopFailing: []overviewFailure{}, Attention: []overviewAttention{}, Environments: []overviewEnvironment{},
	}
	for _, d := range o.Days {
		out.Days = append(out.Days, overviewDay(d))
	}
	for _, f := range o.TopFailing {
		out.TopFailing = append(out.TopFailing, overviewFailure(f))
	}
	for _, a := range o.Attention {
		out.Attention = append(out.Attention, overviewAttention(a))
	}
	for _, e := range o.Environments {
		out.Environments = append(out.Environments, overviewEnvironment(e))
	}
	return out
}
