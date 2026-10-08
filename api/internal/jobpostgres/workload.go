package jobpostgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	WorkloadFeatureSchemaVersion = "rin-workload-features/v1"
	WorkloadProfileVersion       = "rin-workload-profile/v1"
	workloadSampleLimit          = 200
)

type WorkloadRecord struct {
	JobID               string
	ProfileVersion      string
	ProfileKey          string
	AdmissionProfileKey string
	FeatureStage        string
	Features            json.RawMessage
	RefinedAt           *time.Time
	UpdatedAt           time.Time
}

type WorkloadAdmission struct {
	ContentKind    string
	DocumentEngine string
	ResourceClass  string
	PriorityClass  string
	ProjectBytes   int64
	FileCount      *int64
	DocumentClass  string
}

type WorkloadRefinement struct {
	DocumentEngine string
	DocumentClass  string
	FileCount      int64
	PageCount      int64
	MathCount      int64
	DiagramCount   int64
	CodeBlockCount int64
}

type WorkloadProfile struct {
	ProfileKey      string
	RendererVersion string
	SampleCount     int64
	P50MS           *int64
	P90MS           *int64
	MeanMS          *int64
	SamplesMS       []int64
	FailureCount    int64
	FailureCounts   map[string]int64
	UpdatedAt       time.Time
}

type workloadFeatures struct {
	SchemaVersion  string `json:"schemaVersion"`
	Stage          string `json:"stage"`
	DocumentClass  string `json:"documentClass"`
	CostBucket     string `json:"costBucket"`
	DiagramClass   string `json:"diagramClass"`
	ResourceClass  string `json:"resourceClass"`
	PriorityClass  string `json:"priorityClass"`
	ProjectBytes   int64  `json:"projectBytes"`
	FileCount      *int64 `json:"fileCount,omitempty"`
	PageCount      *int64 `json:"pageCount,omitempty"`
	MathCount      *int64 `json:"mathCount,omitempty"`
	DiagramCount   *int64 `json:"diagramCount,omitempty"`
	CodeBlockCount *int64 `json:"codeBlockCount,omitempty"`
}

func AdmissionWorkload(input WorkloadAdmission) (WorkloadRecord, error) {
	documentClass := input.DocumentClass
	if documentClass == "" {
		documentClass = "unknown"
	}
	features := workloadFeatures{
		SchemaVersion: WorkloadFeatureSchemaVersion, Stage: "admission",
		DocumentClass: documentClass, DiagramClass: "unknown", ProjectBytes: input.ProjectBytes,
		ResourceClass: input.ResourceClass, PriorityClass: input.PriorityClass, FileCount: input.FileCount,
	}
	features.CostBucket = workloadCostBucket(features)
	return workloadRecord(input.ContentKind, input.DocumentEngine, features, nil)
}

func RefinedWorkload(contentKind, documentEngine, resourceClass, priorityClass string, projectBytes int64, refinement WorkloadRefinement, now time.Time) (WorkloadRecord, error) {
	features := workloadFeatures{
		SchemaVersion: WorkloadFeatureSchemaVersion, Stage: "analysis",
		DocumentClass: refinement.DocumentClass, ProjectBytes: projectBytes,
		ResourceClass: resourceClass, PriorityClass: priorityClass,
		FileCount: countPointer(refinement.FileCount), PageCount: countPointer(refinement.PageCount),
		MathCount: countPointer(refinement.MathCount), DiagramCount: countPointer(refinement.DiagramCount),
		CodeBlockCount: countPointer(refinement.CodeBlockCount),
	}
	if refinement.DiagramCount > 0 {
		features.DiagramClass = "present"
	} else {
		features.DiagramClass = "none"
	}
	features.CostBucket = workloadCostBucket(features)
	return workloadRecord(contentKind, documentEngine, features, &now)
}

func workloadRecord(contentKind, documentEngine string, features workloadFeatures, refinedAt *time.Time) (WorkloadRecord, error) {
	if contentKind != "latex" && contentKind != "markdown" && contentKind != "typst" {
		return WorkloadRecord{}, errors.New("workload content kind is invalid")
	}
	if !safeProfilePart(documentEngine) || features.ProjectBytes < 0 || features.ProjectBytes > 1<<40 {
		return WorkloadRecord{}, errors.New("workload engine or project bytes are invalid")
	}
	if features.Stage == "admission" {
		if (features.DocumentClass != "unknown" && features.DocumentClass != "article" && features.DocumentClass != "book") || features.DiagramClass != "unknown" || refinedAt != nil ||
			features.PageCount != nil || features.MathCount != nil || features.DiagramCount != nil || features.CodeBlockCount != nil {
			return WorkloadRecord{}, errors.New("admission workload features are invalid")
		}
		if features.FileCount != nil && (*features.FileCount < 0 || *features.FileCount > 1_000_000) {
			return WorkloadRecord{}, errors.New("admission workload file count is invalid")
		}
	} else if features.Stage == "analysis" {
		if (features.DocumentClass != "article" && features.DocumentClass != "book") ||
			(features.DiagramClass != "none" && features.DiagramClass != "present") || refinedAt == nil {
			return WorkloadRecord{}, errors.New("analysis workload features are invalid")
		}
		for _, value := range []*int64{features.FileCount, features.PageCount, features.MathCount, features.DiagramCount, features.CodeBlockCount} {
			if value == nil || *value < 0 || *value > 1_000_000 {
				return WorkloadRecord{}, errors.New("analysis workload count is invalid")
			}
		}
	} else {
		return WorkloadRecord{}, errors.New("workload feature stage is invalid")
	}
	if !safeProfilePart(features.ResourceClass) || (features.PriorityClass != "publish" && features.PriorityClass != "preview" && features.PriorityClass != "rebuild" && features.PriorityClass != "migration") {
		return WorkloadRecord{}, errors.New("workload scheduling class is invalid")
	}
	if features.CostBucket != "small" && features.CostBucket != "medium" && features.CostBucket != "large" && features.CostBucket != "xlarge" {
		return WorkloadRecord{}, errors.New("workload cost bucket is invalid")
	}
	body, err := json.Marshal(features)
	if err != nil {
		return WorkloadRecord{}, err
	}
	key := strings.Join([]string{
		WorkloadProfileVersion, contentKind, documentEngine, features.DocumentClass,
		features.CostBucket, "diagrams-" + features.DiagramClass,
	}, "/")
	record := WorkloadRecord{ProfileVersion: WorkloadProfileVersion, ProfileKey: key, FeatureStage: features.Stage, Features: body, RefinedAt: refinedAt}
	if features.Stage == "admission" {
		record.AdmissionProfileKey = key
	}
	return record, nil
}

func validateAdmissionWorkload(record WorkloadRecord, input WorkloadAdmission) error {
	var features workloadFeatures
	if len(record.Features) == 0 || json.Unmarshal(record.Features, &features) != nil {
		return errors.New("admission workload features are invalid")
	}
	want, err := workloadRecord(input.ContentKind, input.DocumentEngine, features, nil)
	if err != nil || features.ProjectBytes != input.ProjectBytes || features.ResourceClass != input.ResourceClass || features.PriorityClass != input.PriorityClass || record.ProfileVersion != want.ProfileVersion ||
		record.ProfileKey != want.ProfileKey || record.FeatureStage != want.FeatureStage {
		return errors.New("admission workload identity is invalid")
	}
	return nil
}

func workloadCostBucket(features workloadFeatures) string {
	level := 0
	for threshold, candidate := range []int64{256 << 10, 4 << 20, 16 << 20} {
		if features.ProjectBytes > candidate {
			level = threshold + 1
		}
	}
	if features.Stage == "analysis" || features.FileCount != nil {
		levels := []struct {
			value                 *int64
			medium, large, xlarge int64
		}{
			{features.FileCount, 8, 40, 200}, {features.PageCount, 5, 20, 100},
			{features.MathCount, 40, 200, 1000}, {features.DiagramCount, 0, 10, 50},
			{features.CodeBlockCount, 10, 50, 200},
		}
		for _, item := range levels {
			if item.value == nil {
				continue
			}
			switch {
			case *item.value > item.xlarge:
				if level < 3 {
					level = 3
				}
			case *item.value > item.large:
				if level < 2 {
					level = 2
				}
			case *item.value > item.medium:
				if level < 1 {
					level = 1
				}
			}
		}
	}
	return []string{"small", "medium", "large", "xlarge"}[level]
}

func safeProfilePart(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func countPointer(value int64) *int64 { return &value }

type RefineWorkloadInput struct {
	AttemptID      string
	LeaseTokenHash string
	Now            time.Time
	Refinement     WorkloadRefinement
}

func (repository *Repository) RefineWorkload(ctx context.Context, input RefineWorkloadInput) (WorkloadRecord, error) {
	if input.AttemptID == "" || input.LeaseTokenHash == "" || input.Now.IsZero() {
		return WorkloadRecord{}, errors.New("workload refinement input is invalid")
	}
	tx, err := repository.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkloadRecord{}, fmt.Errorf("begin workload refinement: %w", err)
	}
	defer tx.Rollback()
	var jobID, contentKind, documentEngine, resourceClass, priorityClass string
	var projectBytes int64
	err = tx.QueryRowContext(ctx, `
		SELECT jobs.id::text, jobs.content_kind, jobs.document_engine, jobs.resource_class,
			jobs.priority_class, jobs.declared_source_bytes
		FROM rin_renderer.render_attempts AS attempts
		JOIN rin_renderer.render_jobs AS jobs ON jobs.id = attempts.job_id
		WHERE attempts.id = $1::uuid AND attempts.lease_token_hash = $2
			AND attempts.state = 'running' AND attempts.lease_expires_at > $3
			AND jobs.state = 'running'
		FOR UPDATE OF attempts, jobs`, input.AttemptID, input.LeaseTokenHash, input.Now).Scan(
		&jobID, &contentKind, &documentEngine, &resourceClass, &priorityClass, &projectBytes,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkloadRecord{}, ErrLeaseLost
	}
	if err != nil {
		return WorkloadRecord{}, fmt.Errorf("lock workload refinement lease: %w", err)
	}
	if input.Refinement.DocumentEngine != "" {
		documentEngine = input.Refinement.DocumentEngine
	}
	var admissionProfileKey string
	if err = tx.QueryRowContext(ctx, `
		SELECT admission_profile_key
		FROM rin_renderer.render_job_workloads
		WHERE job_id = $1::uuid
		FOR UPDATE`, jobID).Scan(&admissionProfileKey); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return WorkloadRecord{}, fmt.Errorf("read admission workload identity: %w", err)
		}
		admission, admissionErr := AdmissionWorkload(WorkloadAdmission{
			ContentKind: contentKind, DocumentEngine: documentEngine,
			ResourceClass: resourceClass, PriorityClass: priorityClass,
			ProjectBytes: projectBytes,
		})
		if admissionErr != nil {
			return WorkloadRecord{}, admissionErr
		}
		admission.JobID = jobID
		if err := upsertJobWorkload(ctx, tx, admission); err != nil {
			return WorkloadRecord{}, err
		}
		admissionProfileKey = admission.ProfileKey
	}
	record, err := RefinedWorkload(contentKind, documentEngine, resourceClass, priorityClass, projectBytes, input.Refinement, input.Now)
	if err != nil {
		return WorkloadRecord{}, err
	}
	record.JobID = jobID
	record.AdmissionProfileKey = admissionProfileKey
	if err := upsertJobWorkload(ctx, tx, record); err != nil {
		return WorkloadRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_events (job_id, event_type, stage, payload)
		VALUES ($1::uuid, 'workload_refined', 'analysis', jsonb_build_object(
			'profileVersion', $2::text, 'profileKey', $3::text))`, jobID, record.ProfileVersion, record.ProfileKey); err != nil {
		return WorkloadRecord{}, fmt.Errorf("record workload refinement event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkloadRecord{}, fmt.Errorf("commit workload refinement: %w", err)
	}
	return record, nil
}

func upsertJobWorkload(ctx context.Context, tx *sql.Tx, record WorkloadRecord) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.render_job_workloads (
			job_id, profile_version, profile_key, admission_profile_key, feature_stage, features, refined_at
		) VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7)
		ON CONFLICT (job_id) DO UPDATE SET
			profile_version = EXCLUDED.profile_version, profile_key = EXCLUDED.profile_key,
			admission_profile_key = EXCLUDED.admission_profile_key,
			feature_stage = EXCLUDED.feature_stage, features = EXCLUDED.features,
			refined_at = EXCLUDED.refined_at, updated_at = clock_timestamp()`,
		record.JobID, record.ProfileVersion, record.ProfileKey, record.AdmissionProfileKey,
		record.FeatureStage, string(record.Features), record.RefinedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert renderer job workload: %w", err)
	}
	return nil
}

func (repository *Repository) Workload(ctx context.Context, jobID string) (WorkloadRecord, error) {
	var record WorkloadRecord
	err := repository.db.QueryRowContext(ctx, `
		SELECT job_id::text, profile_version, profile_key, admission_profile_key, feature_stage, features,
			refined_at, updated_at
		FROM rin_renderer.render_job_workloads WHERE job_id = $1::uuid`, jobID).Scan(
		&record.JobID, &record.ProfileVersion, &record.ProfileKey, &record.AdmissionProfileKey, &record.FeatureStage,
		&record.Features, &record.RefinedAt, &record.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkloadRecord{}, ErrNotFound
	}
	if err != nil {
		return WorkloadRecord{}, fmt.Errorf("read renderer job workload: %w", err)
	}
	return record, nil
}

func (repository *Repository) WorkloadProfile(ctx context.Context, profileKey, rendererVersion string) (WorkloadProfile, error) {
	var profile WorkloadProfile
	var p50, p90, mean sql.NullInt64
	var samplesJSON, failuresJSON []byte
	err := repository.db.QueryRowContext(ctx, `
		SELECT profile_key, renderer_version, sample_count, p50_ms, p90_ms, mean_ms,
			to_json(samples_ms), failure_count, failure_counts, updated_at
		FROM rin_renderer.workload_profiles WHERE profile_key = $1 AND renderer_version = $2`,
		profileKey, rendererVersion).Scan(&profile.ProfileKey, &profile.RendererVersion,
		&profile.SampleCount, &p50, &p90, &mean, &samplesJSON, &profile.FailureCount,
		&failuresJSON, &profile.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkloadProfile{}, ErrNotFound
	}
	if err != nil {
		return WorkloadProfile{}, fmt.Errorf("read renderer workload profile: %w", err)
	}
	if err := json.Unmarshal(samplesJSON, &profile.SamplesMS); err != nil {
		return WorkloadProfile{}, err
	}
	if err := json.Unmarshal(failuresJSON, &profile.FailureCounts); err != nil {
		return WorkloadProfile{}, err
	}
	if p50.Valid {
		profile.P50MS = countPointer(p50.Int64)
	}
	if p90.Valid {
		profile.P90MS = countPointer(p90.Int64)
	}
	if mean.Valid {
		profile.MeanMS = countPointer(mean.Int64)
	}
	return profile, nil
}

func recordWorkloadOutcome(ctx context.Context, tx *sql.Tx, job Job, durationMS int64, succeeded bool, errorCode string) error {
	record, err := jobWorkloadTx(ctx, tx, job)
	if err != nil {
		return err
	}
	profileKeys := []string{record.ProfileKey}
	if record.AdmissionProfileKey != "" && record.AdmissionProfileKey != record.ProfileKey {
		profileKeys = append(profileKeys, record.AdmissionProfileKey)
	}
	sort.Strings(profileKeys)
	for _, profileKey := range profileKeys {
		if err := recordProfileOutcome(ctx, tx, record.ProfileVersion, profileKey, job.RendererVersion, durationMS, succeeded, errorCode); err != nil {
			return err
		}
	}
	return nil
}

func recordProfileOutcome(ctx context.Context, tx *sql.Tx, profileVersion, profileKey, rendererVersion string, durationMS int64, succeeded bool, errorCode string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, profileVersion+"|"+profileKey+"|"+rendererVersion); err != nil {
		return fmt.Errorf("lock renderer workload profile: %w", err)
	}
	profile := WorkloadProfile{
		ProfileKey: profileKey, RendererVersion: rendererVersion,
		SamplesMS: []int64{}, FailureCounts: map[string]int64{},
	}
	var p50, p90, mean sql.NullInt64
	var samplesJSON, failuresJSON []byte
	err := tx.QueryRowContext(ctx, `
		SELECT sample_count, p50_ms, p90_ms, mean_ms, to_json(samples_ms), failure_count, failure_counts
		FROM rin_renderer.workload_profiles WHERE profile_key = $1 AND renderer_version = $2
		FOR UPDATE`, profileKey, rendererVersion).Scan(
		&profile.SampleCount, &p50, &p90, &mean, &samplesJSON, &profile.FailureCount, &failuresJSON,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock renderer workload profile row: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(samplesJSON, &profile.SamplesMS); err != nil {
			return err
		}
		if err := json.Unmarshal(failuresJSON, &profile.FailureCounts); err != nil {
			return err
		}
		if p50.Valid {
			profile.P50MS = countPointer(p50.Int64)
		}
		if p90.Valid {
			profile.P90MS = countPointer(p90.Int64)
		}
		if mean.Valid {
			profile.MeanMS = countPointer(mean.Int64)
		}
	}
	if succeeded {
		profile.SampleCount++
		profile.SamplesMS = appendDurationSample(profile.SamplesMS, durationMS)
		p50Value, p90Value, meanValue := durationStatistics(profile.SamplesMS)
		profile.P50MS, profile.P90MS, profile.MeanMS = &p50Value, &p90Value, &meanValue
	} else {
		profile.FailureCount++
		profile.FailureCounts[failureClass(errorCode)]++
	}
	samples, _ := json.Marshal(profile.SamplesMS)
	failures, _ := json.Marshal(profile.FailureCounts)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO rin_renderer.workload_profiles (
			profile_key, renderer_version, sample_count, p50_ms, p90_ms, mean_ms,
			samples_ms, failure_count, failure_counts
		) VALUES ($1, $2, $3, $4, $5, $6,
			ARRAY(SELECT jsonb_array_elements_text($7::jsonb)::bigint), $8, $9::jsonb)
		ON CONFLICT (profile_key, renderer_version) DO UPDATE SET
			sample_count = EXCLUDED.sample_count, p50_ms = EXCLUDED.p50_ms,
			p90_ms = EXCLUDED.p90_ms, mean_ms = EXCLUDED.mean_ms,
			samples_ms = EXCLUDED.samples_ms, failure_count = EXCLUDED.failure_count,
			failure_counts = EXCLUDED.failure_counts, updated_at = clock_timestamp()`,
		profile.ProfileKey, profile.RendererVersion, profile.SampleCount,
		profile.P50MS, profile.P90MS, profile.MeanMS, string(samples),
		profile.FailureCount, string(failures),
	)
	if err != nil {
		return fmt.Errorf("update renderer workload profile: %w", err)
	}
	return nil
}

func appendDurationSample(samples []int64, durationMS int64) []int64 {
	result := append(append([]int64(nil), samples...), durationMS)
	if len(result) > workloadSampleLimit {
		result = append([]int64(nil), result[len(result)-workloadSampleLimit:]...)
	}
	return result
}

func jobWorkloadTx(ctx context.Context, tx *sql.Tx, job Job) (WorkloadRecord, error) {
	var record WorkloadRecord
	err := tx.QueryRowContext(ctx, `
		SELECT job_id::text, profile_version, profile_key, admission_profile_key, feature_stage, features, refined_at, updated_at
		FROM rin_renderer.render_job_workloads WHERE job_id = $1::uuid`, job.ID).Scan(
		&record.JobID, &record.ProfileVersion, &record.ProfileKey, &record.AdmissionProfileKey, &record.FeatureStage,
		&record.Features, &record.RefinedAt, &record.UpdatedAt,
	)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkloadRecord{}, fmt.Errorf("read renderer job workload for outcome: %w", err)
	}
	record, err = AdmissionWorkload(WorkloadAdmission{
		ContentKind: job.ContentKind, DocumentEngine: job.DocumentEngine,
		ResourceClass: job.ResourceClass, PriorityClass: job.PriorityClass,
		ProjectBytes: job.DeclaredSourceBytes,
	})
	if err != nil {
		return WorkloadRecord{}, err
	}
	record.JobID = job.ID
	if err := upsertJobWorkload(ctx, tx, record); err != nil {
		return WorkloadRecord{}, err
	}
	return record, nil
}

func durationStatistics(samples []int64) (int64, int64, int64) {
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	quantile := func(value float64) int64 {
		index := int(math.Ceil(value*float64(len(sorted)))) - 1
		if index < 0 {
			index = 0
		}
		return sorted[index]
	}
	var sum float64
	for _, sample := range sorted {
		sum += float64(sample)
	}
	return quantile(0.5), quantile(0.9), int64(math.Round(sum / float64(len(sorted))))
}

func failureClass(code string) string {
	switch {
	case strings.Contains(code, "timeout") || strings.Contains(code, "lease_expired"):
		return "timeout"
	case strings.Contains(code, "unavailable") || strings.Contains(code, "storage") || strings.Contains(code, "transient") || strings.Contains(code, "shutdown"):
		return "unavailable"
	case strings.Contains(code, "invalid") || strings.Contains(code, "source"):
		return "invalid"
	default:
		return "internal"
	}
}
