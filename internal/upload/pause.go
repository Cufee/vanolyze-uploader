package upload

import (
	"context"
	"errors"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/riot"
)

const (
	matchPollInterval = 3 * time.Second
	clientAbsentPoll  = 15 * time.Second
	matchSettleDelay  = 5 * time.Second
)

var errMatchPaused = errors.New("scan interrupted by Riot match status")

// watchMatches offers a work context while uploads are allowed. A transition
// to a pause cancels that context, including any scan already using it.
func (a *App) watchMatches(ctx context.Context, permits chan<- context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	var available chan<- context.Context
	var permit context.Context
	var revoke context.CancelCauseFunc
	var pause string
	var settleUntil time.Time

poll:
	for {
		select {
		case <-ctx.Done():
			break poll
		case available <- permit:
			continue
		case <-timer.C:
		}
		state, err := a.matchActivity(ctx)
		if ctx.Err() != nil {
			break
		}

		var reason string
		delay := matchPollInterval
		switch {
		case err != nil || state == riot.MatchUnknown:
			reason = "Riot status unavailable"
		case state == riot.MatchActive:
			reason = "in match"
		case state == riot.ClientAbsent:
			delay = clientAbsentPoll
		}

		if reason != "" {
			if revoke != nil {
				revoke(errMatchPaused)
				revoke, available = nil, nil
			}
			settleUntil = time.Time{}
		} else if pause != "" {
			if settleUntil.IsZero() {
				settleUntil = time.Now().Add(matchSettleDelay)
			}
			if remaining := time.Until(settleUntil); remaining > 0 {
				reason = pause
				delay = min(delay, remaining)
			}
		}

		if reason != pause {
			pause = reason
			a.status.paused(pause)
			if pause == "" {
				a.log.Info("uploads resumed")
			} else {
				a.log.Info("uploads paused", "reason", pause)
			}
		}
		if reason == "" && available == nil {
			permit, revoke = context.WithCancelCause(ctx)
			available = permits
		}
		timer.Reset(delay)
	}
	if revoke != nil {
		revoke(nil)
	}
}
