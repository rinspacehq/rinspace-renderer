package jobpostgres

import (
	"errors"
	"sort"
	"time"
)

const WaitEstimatorVersion = "rin-wait-estimator/v1"

type EstimationPolicy struct {
	MinimumSamples int64
	Scheduling     SchedulingPolicy
}

func (policy EstimationPolicy) Validate() error {
	if policy.MinimumSamples <= 0 {
		return errors.New("renderer estimation minimum samples must be positive")
	}
	return policy.Scheduling.Validate()
}

type EstimatedStartRange struct {
	Earliest time.Time `json:"earliest"`
	Latest   time.Time `json:"latest"`
}

type WaitEstimate struct {
	EstimatedStartAt    time.Time           `json:"estimatedStartAt"`
	EstimatedStartRange EstimatedStartRange `json:"estimatedStartRange"`
	Confidence          string              `json:"confidence"`
	SampleCount         int64               `json:"sampleCount"`
	EstimatorVersion    string              `json:"estimatorVersion"`
	Scope               string              `json:"scope"`
	CalculatedAt        time.Time           `json:"calculatedAt"`
}

type estimateJob struct {
	ID            string
	PrincipalID   string
	OwnerScope    string
	PriorityClass string
	State         string
	QueuedAt      time.Time
	AvailableAt   time.Time
	StartedAt     *time.Time
	SampleCount   int64
	P50MS         *int64
	P90MS         *int64
}

type estimateSimulation struct {
	start     time.Time
	jobsAhead int64
	samples   int64
}

type estimateCompletion struct {
	at        time.Time
	principal string
}

func simulateEstimatedStart(targetID string, jobs []estimateJob, dispatch map[string]int64, policy EstimationPolicy, now time.Time, percentile int) (estimateSimulation, bool) {
	if err := policy.Validate(); err != nil || (percentile != 50 && percentile != 90) {
		return estimateSimulation{}, false
	}
	capacity := policy.Scheduling.Resources.DocumentLight
	if capacity <= 0 {
		return estimateSimulation{}, false
	}
	queued := make([]estimateJob, 0, len(jobs))
	active := make([]estimateCompletion, 0, len(jobs))
	runningByPrincipal := map[string]int64{}
	activeCount := 0
	minimumSamples := int64(^uint64(0) >> 1)
	for _, job := range jobs {
		principal := job.PrincipalID + "\x00" + job.OwnerScope
		switch job.State {
		case "running":
			activeCount++
			runningByPrincipal[principal]++
			duration := job.P50MS
			if percentile == 90 {
				duration = job.P90MS
			}
			if job.SampleCount < policy.MinimumSamples || duration == nil {
				continue
			}
			if job.SampleCount < minimumSamples {
				minimumSamples = job.SampleCount
			}
			elapsed := int64(0)
			if job.StartedAt != nil && now.After(*job.StartedAt) {
				elapsed = now.Sub(*job.StartedAt).Milliseconds()
			}
			remaining := *duration - elapsed
			if remaining < 0 {
				remaining = 0
			}
			active = append(active, estimateCompletion{at: now.Add(time.Duration(remaining) * time.Millisecond), principal: principal})
		case "queued":
			queued = append(queued, job)
		}
	}
	// The caller supplies jobs for one resource class and rewrites this capacity to that class.
	current := now
	startedAhead := int64(0)
	for len(queued) > 0 {
		for activeCount >= int(capacity) {
			if len(active) == 0 {
				return estimateSimulation{}, false
			}
			sort.Slice(active, func(i, j int) bool { return active[i].at.Before(active[j].at) })
			current = active[0].at
			principal := active[0].principal
			active = active[1:]
			activeCount--
			runningByPrincipal[principal]--
			for len(active) > 0 && !active[0].at.After(current) {
				principal = active[0].principal
				active = active[1:]
				activeCount--
				runningByPrincipal[principal]--
			}
		}
		index := nextEstimatedJob(queued, dispatch, runningByPrincipal, policy.Scheduling, current)
		if index < 0 {
			next := nextEstimateEvent(queued, active, current)
			if !next.After(current) {
				return estimateSimulation{}, false
			}
			current = next
			for len(active) > 0 {
				sort.Slice(active, func(i, j int) bool { return active[i].at.Before(active[j].at) })
				if active[0].at.After(current) {
					break
				}
				principal := active[0].principal
				active = active[1:]
				activeCount--
				runningByPrincipal[principal]--
			}
			continue
		}
		job := queued[index]
		if job.ID == targetID {
			if job.SampleCount < policy.MinimumSamples || job.P50MS == nil || job.P90MS == nil {
				return estimateSimulation{}, false
			}
			if job.SampleCount < minimumSamples {
				minimumSamples = job.SampleCount
			}
			return estimateSimulation{start: current, jobsAhead: startedAhead, samples: minimumSamples}, true
		}
		duration := job.P50MS
		if percentile == 90 {
			duration = job.P90MS
		}
		if job.SampleCount < policy.MinimumSamples || duration == nil {
			return estimateSimulation{}, false
		}
		if job.SampleCount < minimumSamples {
			minimumSamples = job.SampleCount
		}
		queued = append(queued[:index], queued[index+1:]...)
		dispatch[job.PriorityClass]++
		principal := job.PrincipalID + "\x00" + job.OwnerScope
		runningByPrincipal[principal]++
		active = append(active, estimateCompletion{at: current.Add(time.Duration(*duration) * time.Millisecond), principal: principal})
		activeCount++
		startedAhead++
	}
	return estimateSimulation{}, false
}

func nextEstimatedJob(jobs []estimateJob, dispatch map[string]int64, running map[string]int64, policy SchedulingPolicy, now time.Time) int {
	best := -1
	for index, job := range jobs {
		if job.AvailableAt.After(now) || running[job.PrincipalID+"\x00"+job.OwnerScope] >= policy.PrincipalRunning {
			continue
		}
		if best < 0 || estimatedJobLess(job, jobs[best], dispatch, policy, now) {
			best = index
		}
	}
	return best
}

func estimatedJobLess(left, right estimateJob, dispatch map[string]int64, policy SchedulingPolicy, now time.Time) bool {
	leftScore := estimateFairScore(left, dispatch, policy, now)
	rightScore := estimateFairScore(right, dispatch, policy, now)
	if leftScore != rightScore {
		return leftScore < rightScore
	}
	leftPriority, rightPriority := priorityRank(left.PriorityClass), priorityRank(right.PriorityClass)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	if !left.QueuedAt.Equal(right.QueuedAt) {
		return left.QueuedAt.Before(right.QueuedAt)
	}
	return left.ID < right.ID
}

func priorityRank(priority string) int {
	switch priority {
	case "publish":
		return 0
	case "preview":
		return 1
	case "rebuild":
		return 2
	default:
		return 3
	}
}

func estimateFairScore(job estimateJob, dispatch map[string]int64, policy SchedulingPolicy, now time.Time) float64 {
	weight := policy.Weights.Migration
	if job.PriorityClass == "publish" {
		weight = policy.Weights.Publish
	} else if job.PriorityClass == "preview" {
		weight = policy.Weights.Preview
	} else if job.PriorityClass == "rebuild" {
		weight = policy.Weights.Rebuild
	}
	aging := int64(now.Sub(job.QueuedAt) / policy.AgingInterval)
	if aging < 0 {
		aging = 0
	}
	if aging > policy.MaxAgingSteps {
		aging = policy.MaxAgingSteps
	}
	return float64(dispatch[job.PriorityClass])/float64(weight) - float64(aging)
}

func nextEstimateEvent(queued []estimateJob, active []estimateCompletion, now time.Time) time.Time {
	var next time.Time
	for _, job := range queued {
		if job.AvailableAt.After(now) && (next.IsZero() || job.AvailableAt.Before(next)) {
			next = job.AvailableAt
		}
	}
	for _, completion := range active {
		if completion.at.After(now) && (next.IsZero() || completion.at.Before(next)) {
			next = completion.at
		}
	}
	return next
}

func estimateConfidence(sampleCount int64) string {
	if sampleCount >= 100 {
		return "high"
	}
	if sampleCount >= 50 {
		return "medium"
	}
	return "low"
}
