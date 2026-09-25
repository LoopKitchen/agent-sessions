package export

import (
	"sort"
	"time"
)

// sessionPlan is what a run does about the session-keyed tables: which
// sessions it takes, which start days that touches, and the watermark it may
// record once everything for those sessions is in BigQuery.
type sessionPlan struct {
	included  []TouchedSession
	days      []Day
	watermark time.Time
	capped    bool
	// tieSplit counts sessions the plan had to leave behind because more
	// than the cap shared one updated_at; see planSessions.
	tieSplit int
}

// planSessions decides the session-keyed half of a run from the touched
// rows the source returned, which are ordered by (updated_at, session_id)
// and were asked for with limit+1 so the plan can tell a cut set from a
// complete one.
//
// The watermark rule has to hold one property: every session with
// updated_at in (old, new] had its partition rewritten by this run. When the
// set is complete, new is the clock floor (the database clock less the
// margin, which covers a transaction that committed after the run read the
// clock), and never earlier than old. When the set is cut, new is the
// largest updated_at the run took, and the rows taken are exactly those
// strictly before the (limit+1)-th row's updated_at: every row sharing the
// taken maximum is inside the taken set, because rows are ordered by
// updated_at and the first row with a larger value was seen.
//
// The set is also cut at maxDays distinct start days, because a run's cost
// is per partition and the touched sessions of one hour can be spread over
// the whole history: a production corpus of over ten thousand sessions
// spanning some 260 start days means a first run capped at 2,000 sessions alone
// could still be 780 partition files. The cut falls before the first row
// that would open one day too many, and the same strictly-before rule
// applies to it.
//
// A cut set also stops at the clock floor: the watermark it records is the
// largest updated_at taken, and a row stamped inside the margin (a runner
// full pass stamps thousands of sessions within minutes) would put that
// watermark past the floor, where a transaction that began before the run
// read the clock can still commit an updated_at below it and be skipped
// until something touches the session again (review-1 M1). So the rows
// taken are those strictly before both the cut and the floor, and the
// watermark never lands inside the margin. When that leaves nothing (every
// row up to the cut is inside the margin), the run takes nothing and keeps
// the old watermark: the same rows are outside the margin next hour.
//
// The one case the strictly-before rule cannot serve is every row up to
// the cut sharing a single updated_at below the floor: nothing is strictly
// before the cut, and waiting would not change that, so taking nothing would
// never advance. The plan then takes the rows up to the cut and advances
// past the tie, reported in tieSplit so the caller logs it. It needs a
// single statement to stamp more sessions than the cap with one now(); the
// runner's batches are fifty sessions and the cap is two thousand.
func planSessions(rows []TouchedSession, limit, maxDays int, old, floor time.Time) sessionPlan {
	p := sessionPlan{watermark: laterOf(old, floor)}
	cut := len(rows)
	if len(rows) > limit {
		cut = limit
	}
	if maxDays > 0 {
		days := map[Day]bool{}
		for i, r := range rows[:cut] {
			d := DayOf(r.StartedAt)
			if days[d] {
				continue
			}
			if len(days) == maxDays {
				cut = i
				break
			}
			days[d] = true
		}
	}
	if cut == len(rows) {
		p.included = rows
	} else {
		cutoff := rows[cut].UpdatedAt
		bound := cutoff
		if floor.Before(bound) {
			bound = floor
		}
		for _, r := range rows[:cut] {
			if r.UpdatedAt.Before(bound) {
				p.included = append(p.included, r)
			}
		}
		p.capped = true
		switch {
		case len(p.included) > 0:
			p.watermark = p.included[len(p.included)-1].UpdatedAt
		case cutoff.Before(floor):
			// The tie: every row up to the cut carries cutoff, below the
			// floor.
			p.included = rows[:cut]
			p.tieSplit = len(rows) - cut
			p.watermark = cutoff
		default:
			// Everything taken would be inside the margin: nothing this
			// hour, and the old position stands.
			p.watermark = old
		}
	}
	seen := map[Day]bool{}
	for _, r := range p.included {
		d := DayOf(r.StartedAt)
		if !seen[d] {
			seen[d] = true
			p.days = append(p.days, d)
		}
	}
	sort.Slice(p.days, func(i, j int) bool { return p.days[i] < p.days[j] })
	return p
}

// dayPlan is the event half: the days rewritten and the watermark recorded.
type dayPlan struct {
	days      []Day
	watermark time.Time
	capped    bool
}

// planDays decides which event days a run rewrites from the ascending days
// the source returned, asked for with limit+1. Complete: every day, and the
// watermark is the clock floor. Cut: the first limit days, and the watermark
// is the last microsecond of the last day taken, so the next run's
// "ingested_at > watermark" starts at the first microsecond of the day
// after it. Postgres timestamps are microseconds, so the boundary is exact.
func planDays(days []Day, limit int, old, floor time.Time) dayPlan {
	p := dayPlan{watermark: laterOf(old, floor)}
	if len(days) <= limit {
		p.days = days
		return p
	}
	p.days = days[:limit]
	p.capped = true
	p.watermark = p.days[len(p.days)-1].Next().Start().Add(-time.Microsecond)
	return p
}
