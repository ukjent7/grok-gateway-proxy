package store

import (
	"context"
	"time"
)

func mod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

type TimeBucket struct {
	Start           time.Time `json:"t"`
	Requests        int64     `json:"requests"`
	Failures        int64     `json:"failures"`
	PromptTokens    int64     `json:"prompt_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CacheReadTokens int64     `json:"cache_read_tokens"`
}

type TimeSeries struct {
	From          time.Time    `json:"from"`
	To            time.Time    `json:"to"`
	BucketSeconds int64        `json:"bucket_seconds"`
	Buckets       []TimeBucket `json:"buckets"`
}

func (s *Store) TimeSeries(ctx context.Context, filter LogFilter, buckets int) (TimeSeries, error) {
	if buckets < 1 {
		buckets = 1
	}
	if buckets > 240 {
		buckets = 240
	}

	now := time.Now().UTC()
	to := now
	if filter.To != nil && filter.To.Before(now) {
		to = filter.To.UTC()
	}

	if sub := to.Nanosecond(); sub != 0 {
		to = to.Add(time.Second - time.Duration(sub))
	}
	from := to
	if filter.From != nil {
		from = filter.From.UTC()
	} else {
		var earliest string

		err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(MIN(started_at), '') FROM request_logs WHERE started_at != ''`).Scan(&earliest)
		if err != nil {
			return TimeSeries{}, err
		}
		if earliest != "" {
			from = parseTimestamp(earliest)
		}
	}
	if from.After(to) {
		from = to
	}

	total := int64(to.Sub(from) / time.Second)
	step := (total + int64(buckets) - 1) / int64(buckets)
	if step < 1 {
		step = 1
	}

	where, args := buildLogFilter(filter)

	if filter.To == nil {
		filter.To = &to
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT (CAST(strftime('%s', started_at) AS INTEGER) / ?) * ? AS bucket,
		        COUNT(*),
		        COALESCE(SUM(CASE WHEN success = 1 THEN 0 ELSE 1 END), 0),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(reasoning_tokens), 0),
		        COALESCE(SUM(cache_read_tokens), 0)
		 FROM request_logs`+where+` AND started_at != ''
		 GROUP BY bucket ORDER BY bucket`,
		append([]any{step, step}, args...)...)
	if err != nil {
		return TimeSeries{}, err
	}
	defer rows.Close()

	seen := make(map[int64]TimeBucket)
	for rows.Next() {
		var epoch int64
		var bucket TimeBucket
		if err := rows.Scan(&epoch, &bucket.Requests, &bucket.Failures,
			&bucket.PromptTokens, &bucket.OutputTokens, &bucket.ReasoningTokens,
			&bucket.CacheReadTokens); err != nil {
			return TimeSeries{}, err
		}
		bucket.Start = time.Unix(epoch, 0).UTC()
		seen[epoch] = bucket
	}
	if err := rows.Err(); err != nil {
		return TimeSeries{}, err
	}

	alignedEpoch := from.Unix() - mod(from.Unix(), step)
	bucketsOut := make([]TimeBucket, 0, buckets)
	for epoch := alignedEpoch; epoch < to.Unix(); epoch += step {
		if bucket, ok := seen[epoch]; ok {
			bucketsOut = append(bucketsOut, bucket)
			continue
		}
		bucketsOut = append(bucketsOut, TimeBucket{Start: time.Unix(epoch, 0).UTC()})
	}

	return TimeSeries{
		From:          time.Unix(alignedEpoch, 0).UTC(),
		To:            to,
		BucketSeconds: step,
		Buckets:       bucketsOut,
	}, nil
}
