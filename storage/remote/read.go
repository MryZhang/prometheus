// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"
)

// QueryableClient returns a storage.Queryable which queries the given
// Client to select series sets.
func QueryableClient(c *Client) storage.Queryable {
	return storage.QuerierFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		return &querier{
			ctx:    ctx,
			mint:   mint,
			maxt:   maxt,
			client: c,
		}, nil
	})
}

// querier is an adapter to make a Client usable as a storage.Querier.
type querier struct {
	ctx        context.Context
	mint, maxt int64
	client     *Client
}

// Select implements storage.Querier and uses the given matchers to read series
// sets from the Client.
func (q *querier) Select(matchers ...*labels.Matcher) storage.SeriesSet {
	query, err := ToQuery(q.mint, q.maxt, matchers)
	if err != nil {
		return errSeriesSet{err: err}
	}

	res, err := q.client.Read(q.ctx, query)
	if err != nil {
		return errSeriesSet{err: err}
	}

	return FromQueryResult(res)
}

// LabelValues implements storage.Querier and is a noop.
func (q *querier) LabelValues(name string) ([]string, error) {
	// TODO implement?
	return nil, nil
}

// Close implements storage.Querier and is a noop.
func (q *querier) Close() error {
	return nil
}

// ExternablLabelsHandler returns a storage.Queryable which creates a
// externalLabelsQuerier.
func ExternablLabelsHandler(next storage.Queryable, externalLabels model.LabelSet) storage.Queryable {
	return storage.QuerierFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		q, err := next.Querier(ctx, mint, maxt)
		if err != nil {
			return nil, err
		}
		return &externalLabelsQuerier{Querier: q, externalLabels: externalLabels}, nil
	})
}

// externalLabelsQuerier is a querier which ensures that Select() results match
// the configured external labels.
type externalLabelsQuerier struct {
	storage.Querier

	externalLabels model.LabelSet
}

// Select adds equality matchers for all external labels to the list of matchers
// before calling the wrapped storage.Queryable. The added external labels are
// removed from the returned series sets.
func (q externalLabelsQuerier) Select(matchers ...*labels.Matcher) storage.SeriesSet {
	m, added := addExternalLabels(q.externalLabels, matchers)
	s := q.Select(m...)
	return newSeriesSetFilter(s, added)
}

// PreferLocalStorageFilter returns a QuerierFunc which creates a NoopQuerier if
// requested timeframe can be answered completely by the local TSDB, and reduces
// maxt if the timeframe can be partially answered by TSDB.
func PreferLocalStorageFilter(next storage.Queryable, cb startTimeCallback) storage.Queryable {
	return storage.QuerierFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		localStartTime, err := cb()
		if err != nil {
			return nil, err
		}
		cmaxt := maxt
		// Avoid queries whose timerange is later than the first timestamp in local DB.
		if mint > localStartTime {
			return storage.NoopQuerier(), nil
		}
		// Query only samples older than the first timestamp in local DB.
		if maxt > localStartTime {
			cmaxt = localStartTime
		}
		return next.Querier(ctx, mint, cmaxt)
	})
}

// RequiredLabelsFilter returns a storage.Queryable which creates a
// requiredLabelsQuerier.
func RequiredLabelsFilter(next storage.Queryable, requiredLabels model.LabelSet) storage.Queryable {
	return storage.QuerierFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		q, err := next.Querier(ctx, mint, maxt)
		if err != nil {
			return nil, err
		}
		return &requiredLabelsQuerier{Querier: q, requiredLabels: requiredLabels}, nil
	})
}

// requiredLabelsQuerier wraps a storage.Querier and requires Select() calls to
// match the given labelSet.
type requiredLabelsQuerier struct {
	storage.Querier

	requiredLabels model.LabelSet
}

// Select returns a NoopSeriesSet if the given matchers don't match the label
// set of the requiredLabelsQuerier. Otherwise it'll call the wrapped querier.
func (q requiredLabelsQuerier) Select(matchers ...*labels.Matcher) storage.SeriesSet {
	ls := q.requiredLabels
	for _, m := range matchers {
		for k, v := range ls {
			if m.Name == string(k) && m.Matches(string(v)) {
				delete(ls, k)
				break
			}
		}
		if len(ls) == 0 {
			break
		}
	}
	if len(ls) > 0 {
		return storage.NoopSeriesSet()
	}
	return q.Querier.Select(matchers...)
}

// addExternalLabels adds matchers for each external label. External labels
// that already have a corresponding user-supplied matcher are skipped, as we
// assume that the user explicitly wants to select a different value for them.
// We return the new set of matchers, along with a map of labels for which
// matchers were added, so that these can later be removed from the result
// time series again.
func addExternalLabels(externalLabels model.LabelSet, ms []*labels.Matcher) ([]*labels.Matcher, model.LabelSet) {
	el := make(model.LabelSet, len(externalLabels))
	for k, v := range externalLabels {
		el[k] = v
	}
	for _, m := range ms {
		if _, ok := el[model.LabelName(m.Name)]; ok {
			delete(el, model.LabelName(m.Name))
		}
	}
	for k, v := range el {
		m, err := labels.NewMatcher(labels.MatchEqual, string(k), string(v))
		if err != nil {
			panic(err)
		}
		ms = append(ms, m)
	}
	return ms, el
}

func newSeriesSetFilter(ss storage.SeriesSet, toFilter model.LabelSet) storage.SeriesSet {
	return &seriesSetFilter{
		SeriesSet: ss,
		toFilter:  toFilter,
	}
}

type seriesSetFilter struct {
	storage.SeriesSet
	toFilter model.LabelSet
}

func (ssf seriesSetFilter) At() storage.Series {
	return seriesFilter{
		Series:   ssf.SeriesSet.At(),
		toFilter: ssf.toFilter,
	}
}

type seriesFilter struct {
	storage.Series
	toFilter model.LabelSet
}

func (sf seriesFilter) Labels() labels.Labels {
	labels := sf.Series.Labels()
	for i := 0; i < len(labels); {
		if _, ok := sf.toFilter[model.LabelName(labels[i].Name)]; ok {
			labels = labels[:i+copy(labels[i:], labels[i+1:])]
			continue
		}
		i++
	}
	return labels
}
