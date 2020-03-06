/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package tide contains a controller for managing a tide pool of PRs. The
// controller will automatically retest PRs in the pool and merge them if they
// pass tests.
package tide

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx/pkg/tekton/metapipeline"
	"github.com/jenkins-x/lighthouse/pkg/io"
	"github.com/jenkins-x/lighthouse/pkg/plumber"
	"github.com/jenkins-x/lighthouse/pkg/prow/config"
	"github.com/jenkins-x/lighthouse/pkg/prow/errorutil"
	"github.com/jenkins-x/lighthouse/pkg/prow/git"
	"github.com/jenkins-x/lighthouse/pkg/prow/gitprovider"
	"github.com/jenkins-x/lighthouse/pkg/prow/pjutil"
	"github.com/jenkins-x/lighthouse/pkg/tide/blockers"
	"github.com/jenkins-x/lighthouse/pkg/tide/history"
	"github.com/jenkins-x/lighthouse/pkg/util"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	githubql "github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"
	tektonclient "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// For mocking out sleep during unit tests.
var sleep = time.Sleep

type plumberClient interface {
	Create(*plumber.PipelineOptions, metapipeline.Client, scm.Repository) (*plumber.PipelineOptions, error)
	List(opts metav1.ListOptions) (*plumber.PipelineOptionsList, error)
}

type scmProviderClient interface {
	CreateGraphQLStatus(string, string, string, *gitprovider.Status) (*scm.Status, error)
	GetCombinedStatus(org, repo, ref string) (*scm.CombinedStatus, error)
	CreateStatus(org, repo, ref string, s *scm.StatusInput) (*scm.Status, error)
	GetPullRequestChanges(org, repo string, number int) ([]*scm.Change, error)
	GetRef(string, string, string) (string, error)
	Merge(string, string, int, gitprovider.MergeDetails) error
	Query(context.Context, interface{}, map[string]interface{}) error
}

type contextChecker interface {
	// IsOptional tells whether a context is optional.
	IsOptional(string) bool
	// MissingRequiredContexts tells if required contexts are missing from the list of contexts provided.
	MissingRequiredContexts([]string) []string
}

// DefaultController knows how to sync PRs and PJs.
type DefaultController struct {
	logger        *logrus.Entry
	config        config.Getter
	spc           scmProviderClient
	plumberClient plumberClient
	gc            git.Client
	mpClient      metapipeline.Client
	tektonClient  tektonclient.Interface
	ns            string

	sc *statusController

	m     sync.Mutex
	pools []Pool

	// changedFiles caches the names of files changed by PRs.
	// Cache entries expire if they are not used during a sync loop.
	changedFiles *changedFilesAgent

	History *history.History
}

// Action represents what actions the controller can take. It will take
// exactly one action each sync.
type Action string

// Constants for various actions the controller might take
const (
	Wait         Action = "WAIT"
	Trigger             = "TRIGGER"
	TriggerBatch        = "TRIGGER_BATCH"
	Merge               = "MERGE"
	MergeBatch          = "MERGE_BATCH"
	PoolBlocked         = "BLOCKED"
)

// recordableActions is the subset of actions that we keep historical record of.
// Ignore idle actions to avoid flooding the records with useless data.
var recordableActions = map[Action]bool{
	Trigger:      true,
	TriggerBatch: true,
	Merge:        true,
	MergeBatch:   true,
}

// Pool represents information about a tide pool. There is one for every
// org/repo/branch combination that has PRs in the pool.
type Pool struct {
	Org    string
	Repo   string
	Branch string

	// PRs with passing tests, pending tests, and missing or failed tests.
	// Note that these results are rolled up. If all tests for a PR are passing
	// except for one pending, it will be in PendingPRs.
	SuccessPRs []PullRequest
	PendingPRs []PullRequest
	MissingPRs []PullRequest

	// Empty if there is no pending batch.
	BatchPending []PullRequest

	// Which action did we last take, and to what target(s), if any.
	Action   Action
	Target   []PullRequest
	Blockers []blockers.Blocker
	Error    string
}

// Prometheus Metrics
var (
	tideMetrics = struct {
		// Per pool
		pooledPRs  *prometheus.GaugeVec
		updateTime *prometheus.GaugeVec
		merges     *prometheus.HistogramVec

		// Singleton
		syncDuration         prometheus.Gauge
		statusUpdateDuration prometheus.Gauge
	}{
		pooledPRs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "pooledprs",
			Help: "Number of PRs in each Tide pool.",
		}, []string{
			"org",
			"repo",
			"branch",
		}),
		updateTime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "updatetime",
			Help: "The last time each subpool was synced. (Used to determine 'pooledprs' freshness.)",
		}, []string{
			"org",
			"repo",
			"branch",
		}),

		merges: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "merges",
			Help:    "Histogram of merges where values are the number of PRs merged together.",
			Buckets: []float64{1, 2, 3, 4, 5, 7, 10, 15, 25},
		}, []string{
			"org",
			"repo",
			"branch",
		}),

		syncDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "syncdur",
			Help: "The duration of the last loop of the sync controller.",
		}),

		statusUpdateDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "statusupdatedur",
			Help: "The duration of the last loop of the status update controller.",
		}),
	}
)

func init() {
	prometheus.MustRegister(tideMetrics.pooledPRs)
	prometheus.MustRegister(tideMetrics.updateTime)
	prometheus.MustRegister(tideMetrics.merges)
	prometheus.MustRegister(tideMetrics.syncDuration)
	prometheus.MustRegister(tideMetrics.statusUpdateDuration)
}

// NewController makes a DefaultController out of the given clients.
func NewController(spcSync, spcStatus *gitprovider.Client, plumberClient plumberClient, mpClient metapipeline.Client, tektonClient tektonclient.Interface, ns string, cfg config.Getter, gc git.Client, maxRecordsPerPool int, opener io.Opener, historyURI, statusURI string, logger *logrus.Entry) (*DefaultController, error) {
	if logger == nil {
		logger = logrus.NewEntry(logrus.StandardLogger())
	}
	hist, err := history.New(maxRecordsPerPool, opener, historyURI)
	if err != nil {
		return nil, fmt.Errorf("error initializing history client from %q: %v", historyURI, err)
	}
	sc := &statusController{
		logger:         logger.WithField("controller", "status-update"),
		spc:            spcStatus,
		config:         cfg,
		newPoolPending: make(chan bool, 1),
		shutDown:       make(chan bool),
		opener:         opener,
		path:           statusURI,
	}
	go sc.run()
	return &DefaultController{
		logger:        logger.WithField("controller", "sync"),
		spc:           spcSync,
		plumberClient: plumberClient,
		mpClient:      mpClient,
		tektonClient:  tektonClient,
		ns:            ns,
		config:        cfg,
		gc:            gc,
		sc:            sc,
		changedFiles: &changedFilesAgent{
			spc:             spcSync,
			nextChangeCache: make(map[changeCacheKey][]string),
		},
		History: hist,
	}, nil
}

// Shutdown signals the statusController to stop working and waits for it to
// finish its last update loop before terminating.
// DefaultController.Sync() should not be used after this function is called.
func (c *DefaultController) Shutdown() {
	err := c.gc.Clean()
	if err != nil {
		c.logger.Warnf("error cleaning local git cache: %s", err)
	}
	c.History.Flush()
	c.sc.shutdown()
}

// GetHistory returns the history
func (c *DefaultController) GetHistory() *history.History {
	return c.History
}

func prKey(pr *PullRequest) string {
	return fmt.Sprintf("%s#%d", string(pr.Repository.NameWithOwner), int(pr.Number))
}

// org/repo#number -> pr
func byRepoAndNumber(prs []PullRequest) map[string]PullRequest {
	m := make(map[string]PullRequest)
	for _, pr := range prs {
		key := prKey(&pr)
		m[key] = pr
	}
	return m
}

// newExpectedContext creates a Context with Expected state.
func newExpectedContext(c string) Context {
	return Context{
		Context:     githubql.String(c),
		State:       githubql.StatusStateExpected,
		Description: githubql.String(""),
	}
}

// contextsToStrings converts a list Context to a list of string
func contextsToStrings(contexts []Context) []string {
	var names []string
	for _, c := range contexts {
		names = append(names, string(c.Context))
	}
	return names
}

// Sync runs one sync iteration.
func (c *DefaultController) Sync() error {
	start := time.Now()
	defer func() {
		duration := time.Since(start)
		c.logger.WithField("duration", duration.String()).Info("Synced")
		tideMetrics.syncDuration.Set(duration.Seconds())
	}()
	defer c.changedFiles.prune()

	c.logger.Debug("Building tide pool.")
	prs := make(map[string]PullRequest)
	for _, query := range c.config().Tide.Queries {
		q := query.Query()
		results, err := search(c.spc.Query, c.logger, q, time.Time{}, time.Now())
		if err != nil && len(results) == 0 {
			return fmt.Errorf("query %q, err: %v", q, err)
		}
		if err != nil {
			c.logger.WithError(err).WithField("query", q).Warning("found partial results")
		}
		for _, pr := range results {
			prs[prKey(&pr)] = pr
		}
	}
	c.logger.WithField(
		"duration", time.Since(start).String(),
	).Debugf("Found %d (unfiltered) pool PRs.", len(prs))

	var pjs []plumber.PipelineOptions
	var blocks blockers.Blockers
	var err error
	if len(prs) > 0 {
		start := time.Now()
		pjList, err := c.plumberClient.List(metav1.ListOptions{})
		if err != nil {
			c.logger.WithField("duration", time.Since(start).String()).Debug("Failed to list PipelineActivitys from the cluster.")
			return err
		}
		c.logger.WithField("duration", time.Since(start).String()).Debug("Listed PipelineActivitys from the cluster.")
		pjs = pjList.Items

		if label := c.config().Tide.BlockerLabel; label != "" {
			c.logger.Debugf("Searching for blocking issues (label %q).", label)
			orgExcepts, repos := c.config().Tide.Queries.OrgExceptionsAndRepos()
			orgs := make([]string, 0, len(orgExcepts))
			for org := range orgExcepts {
				orgs = append(orgs, org)
			}
			orgRepoQuery := orgRepoQueryString(orgs, repos.UnsortedList(), orgExcepts)
			blocks, err = blockers.FindAll(c.spc, c.logger, label, orgRepoQuery)
			if err != nil {
				return err
			}
		}
	}
	// Partition PRs into subpools and filter out non-pool PRs.
	rawPools, err := c.dividePool(prs, pjs)
	if err != nil {
		return err
	}
	filteredPools := c.filterSubpools(c.config().Tide.MaxGoroutines, rawPools)

	// Notify statusController about the new pool.
	c.sc.Lock()
	c.sc.blocks = blocks
	c.sc.poolPRs = poolPRMap(filteredPools)
	select {
	case c.sc.newPoolPending <- true:
	default:
	}
	c.sc.Unlock()

	// Sync subpools in parallel.
	poolChan := make(chan Pool, len(filteredPools))
	subpoolsInParallel(
		c.config().Tide.MaxGoroutines,
		filteredPools,
		func(sp *subpool) {
			pool, err := c.syncSubpool(*sp, blocks.GetApplicable(sp.org, sp.repo, sp.branch))
			if err != nil {
				sp.log.WithError(err).Errorf("Error syncing subpool.")
			}
			poolChan <- pool
		},
	)

	close(poolChan)
	pools := make([]Pool, 0, len(poolChan))
	for pool := range poolChan {
		pools = append(pools, pool)
	}
	sortPools(pools)
	c.m.Lock()
	c.pools = pools
	// While we're locked, rerun failed-but-rerunnable PipelineRuns.
	c.logger.WithField("duration", time.Since(start).String()).Debug("Rerunning PipelineRuns failed due to race condition.")
	err = rerunPipelineRunsWithRaceConditionFailure(c.tektonClient, c.ns, c.logger)
	if err != nil {
		c.logger.WithError(err).Error("Error rerunning PipelineRuns failed by Tekton race condition")
	}
	c.logger.WithField("duration", time.Since(start).String()).Debug("Finished rerunning PipelineRuns failed due to race condition.")
	c.m.Unlock()

	c.History.Flush()
	return nil
}

func (c *DefaultController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.m.Lock()
	defer c.m.Unlock()
	b, err := json.Marshal(c.pools)
	if err != nil {
		c.logger.WithError(err).Error("Encoding JSON.")
		b = []byte("[]")
	}
	if _, err = w.Write(b); err != nil {
		c.logger.WithError(err).Error("Writing JSON response.")
	}
}

// GetPools returns the pool status
func (c *DefaultController) GetPools() []Pool {
	c.m.Lock()
	defer c.m.Unlock()
	answer := []Pool{}
	for _, p := range c.pools {
		answer = append(answer, p)
	}
	return answer
}

func subpoolsInParallel(goroutines int, sps map[string]*subpool, process func(*subpool)) {
	// Load the subpools into a channel for use as a work queue.
	queue := make(chan *subpool, len(sps))
	for _, sp := range sps {
		queue <- sp
	}
	close(queue)

	if goroutines > len(queue) {
		goroutines = len(queue)
	}
	wg := &sync.WaitGroup{}
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for sp := range queue {
				process(sp)
			}
		}()
	}
	wg.Wait()
}

// filterSubpools filters non-pool PRs out of the initially identified subpools,
// deleting any pools that become empty.
// See filterSubpool for filtering details.
func (c *DefaultController) filterSubpools(goroutines int, raw map[string]*subpool) map[string]*subpool {
	filtered := make(map[string]*subpool)
	var lock sync.Mutex

	subpoolsInParallel(
		goroutines,
		raw,
		func(sp *subpool) {
			if err := c.initSubpoolData(sp); err != nil {
				sp.log.WithError(err).Error("Error initializing subpool.")
				return
			}
			key := poolKey(sp.org, sp.repo, sp.branch)
			if spFiltered := filterSubpool(c.spc, sp); spFiltered != nil {
				sp.log.WithField("key", key).WithField("pool", spFiltered).Debug("filtered sub-pool")

				lock.Lock()
				filtered[key] = spFiltered
				lock.Unlock()
			} else {
				sp.log.WithField("key", key).WithField("pool", spFiltered).Debug("filtering sub-pool removed all PRs")
			}
		},
	)
	return filtered
}

func (c *DefaultController) initSubpoolData(sp *subpool) error {
	var err error
	sp.presubmits, err = c.presubmitsByPull(sp)
	if err != nil {
		return fmt.Errorf("error determining required presubmit PipelineActivitys: %v", err)
	}
	sp.cc, err = c.config().GetTideContextPolicy(sp.org, sp.repo, sp.branch)
	if err != nil {
		return fmt.Errorf("error setting up context checker: %v", err)
	}
	return nil
}

// filterSubpool filters PRs from an initially identified subpool, returning the
// filtered subpool.
// If the subpool becomes empty 'nil' is returned to indicate that the subpool
// should be deleted.
func filterSubpool(spc scmProviderClient, sp *subpool) *subpool {
	var toKeep []PullRequest
	for _, pr := range sp.prs {
		if !filterPR(spc, sp, &pr) {
			toKeep = append(toKeep, pr)
		}
	}
	if len(toKeep) == 0 {
		return nil
	}
	sp.prs = toKeep
	return sp
}

// filterPR indicates if a PR should be filtered out of the subpool.
// Specifically we filter out PRs that:
// - Have known merge conflicts.
// - Have failing or missing status contexts.
// - Have pending required status contexts that are not associated with a
//   PipelineActivity. (This ensures that the 'tide' context indicates that the pending
//   status is preventing merge. Required PipelineActivity statuses are allowed to be
//   'pending' because this prevents kicking PRs from the pool when Tide is
//   retesting them.)
func filterPR(spc scmProviderClient, sp *subpool, pr *PullRequest) bool {
	log := sp.log.WithFields(pr.logFields())
	// Skip PRs that are known to be unmergeable.
	if pr.Mergeable == githubql.MergeableStateConflicting {
		log.Debug("filtering out PR as it is unmergeable")
		return true
	}
	// Filter out PRs with unsuccessful contexts unless the only unsuccessful
	// contexts are pending required PipelineActivitys.
	contexts, err := headContexts(log, spc, pr)
	if err != nil {
		log.WithError(err).Error("Getting head contexts.")
		return true
	}
	presubmitsHaveContext := func(context string) bool {
		for _, job := range sp.presubmits[int(pr.Number)] {
			if job.Context == context {
				return true
			}
		}
		return false
	}
	for _, ctx := range unsuccessfulContexts(contexts, sp.cc, log) {
		if ctx.State != githubql.StatusStatePending {
			log.WithField("context", ctx.Context).Debug("filtering out PR as unsuccessful context is not pending")
			return true
		}
		if !presubmitsHaveContext(string(ctx.Context)) {
			log.WithField("context", ctx.Context).Debug("filtering out PR as unsuccessful context is not Prow-controlled")
			return true
		}
	}

	return false
}

// poolPRMap collects all subpool PRs into a map containing all pooled PRs.
func poolPRMap(subpoolMap map[string]*subpool) map[string]PullRequest {
	prs := make(map[string]PullRequest)
	for _, sp := range subpoolMap {
		for _, pr := range sp.prs {
			prs[prKey(&pr)] = pr
		}
	}
	return prs
}

type simpleState string

const (
	failureState simpleState = "failure"
	pendingState simpleState = "pending"
	successState simpleState = "success"
)

func githubqlStatusStateToSimpleState(gqlState githubql.StatusState) simpleState {
	switch gqlState {
	case githubql.StatusStateSuccess:
		return successState
	case githubql.StatusStateExpected, githubql.StatusStatePending:
		return pendingState
	default:
		return failureState
	}
}

func toSimpleState(s plumber.PipelineState) simpleState {
	if s == plumber.TriggeredState || s == plumber.PendingState || s == plumber.RunningState {
		return pendingState
	} else if s == plumber.SuccessState {
		return successState
	}
	return failureState
}

// isPassingTests returns whether or not all contexts set on the PR except for
// the tide pool context are passing.
func isPassingTests(log *logrus.Entry, spc scmProviderClient, pr PullRequest, cc contextChecker) bool {
	log = log.WithFields(pr.logFields())
	contexts, err := headContexts(log, spc, &pr)
	if err != nil {
		log.WithError(err).Error("Getting head commit status contexts.")
		// If we can't get the status of the commit, assume that it is failing.
		return false
	}
	unsuccessful := unsuccessfulContexts(contexts, cc, log)
	return len(unsuccessful) == 0
}

// unsuccessfulContexts determines which contexts from the list that we care about are
// failed. For instance, we do not care about our own context.
// If the branchProtection is set to only check for required checks, we will skip
// all non-required tests. If required tests are missing from the list, they will be
// added to the list of failed contexts.
func unsuccessfulContexts(contexts []Context, cc contextChecker, log *logrus.Entry) []Context {
	var failed []Context
	for _, ctx := range contexts {
		if string(ctx.Context) == statusContext {
			continue
		}
		if cc.IsOptional(string(ctx.Context)) {
			continue
		}
		if ctx.State != githubql.StatusStateSuccess {
			failed = append(failed, ctx)
		}
	}
	for _, c := range cc.MissingRequiredContexts(contextsToStrings(contexts)) {
		failed = append(failed, newExpectedContext(c))
	}

	log.Debugf("from %d total contexts (%v) found %d failing contexts: %v", len(contexts), contextsToStrings(contexts), len(failed), contextsToStrings(failed))
	return failed
}

func pickSmallestPassingNumber(log *logrus.Entry, spc scmProviderClient, prs []PullRequest, cc contextChecker) (bool, PullRequest) {
	smallestNumber := -1
	var smallestPR PullRequest
	for _, pr := range prs {
		if smallestNumber != -1 && int(pr.Number) >= smallestNumber {
			continue
		}
		if len(pr.Commits.Nodes) < 1 {
			continue
		}
		if !isPassingTests(log, spc, pr, cc) {
			continue
		}
		smallestNumber = int(pr.Number)
		smallestPR = pr
	}
	return smallestNumber > -1, smallestPR
}

// accumulateBatch returns a list of PRs that can be merged after passing batch
// testing, if any exist. It also returns a list of PRs currently being batch
// tested.
func accumulateBatch(presubmits map[int][]config.Presubmit, prs []PullRequest, pjs []plumber.PipelineOptions, spc scmProviderClient, log *logrus.Entry) ([]PullRequest, []PullRequest) {
	log.Debug("accumulating PRs for batch testing")
	if len(presubmits) == 0 {
		log.Debug("no presubmits configured, no batch can be triggered")
		return nil, nil
	}
	prNums := make(map[int]PullRequest)
	for _, pr := range prs {
		prNums[int(pr.Number)] = pr
	}
	type accState struct {
		prs       []PullRequest
		jobStates map[string]simpleState
		// Are the pull requests in the ref still acceptable? That is, do they
		// still point to the heads of the PRs?
		validPulls bool
	}
	states := make(map[string]*accState)
	for _, pj := range pjs {
		if pj.Spec.Type != plumber.BatchJob {
			continue
		}
		// First validate the batch job's refs.
		ref := pj.Spec.Refs.String()
		if _, ok := states[ref]; !ok {
			state := &accState{
				jobStates:  make(map[string]simpleState),
				validPulls: true,
			}
			for _, pull := range pj.Spec.Refs.Pulls {
				if pr, ok := prNums[pull.Number]; ok && string(pr.HeadRefOID) == pull.SHA {
					state.prs = append(state.prs, pr)
				} else if !ok {
					state.validPulls = false
					log.WithField("batch", ref).WithFields(pr.logFields()).Debug("batch job invalid, PR left pool")
					break
				} else {
					state.validPulls = false
					log.WithField("batch", ref).WithFields(pr.logFields()).Debug("batch job invalid, PR HEAD changed")
					break
				}
			}
			states[ref] = state
		}
		if !states[ref].validPulls {
			// The batch contains a PR ref that has changed. Skip it.
			continue
		}

		// Batch job refs are valid. Now accumulate job states by batch ref.
		context := pj.Spec.Context
		jobState := toSimpleState(pj.Status.State)

		// Check for possible override cases by looking for the PR with the PJ's lastCommitSHA and checking its statuses.
		for _, pr := range states[ref].prs {
			if prHeadIsInPJPulls(pr, pj) {
				contextStatuses, err := headContexts(log, spc, &pr)
				if err != nil {
					log.WithError(err).Error("Error getting head contexts, solely using PJs for status")
				}
				// Iterate over contexts and reset the job state if the context matches and is overridden.
				for _, contextInfo := range contextStatuses {
					if string(contextInfo.Context) == context {
						jobState = commitStatusOrPJStatusForContext(contextInfo, jobState)
					}
				}
			}
		}

		// Store the best result for this ref+context.
		if s, ok := states[ref].jobStates[context]; !ok || s == failureState || jobState == successState {
			states[ref].jobStates[context] = jobState
		}
	}
	var pendingBatch, successBatch []PullRequest
	for ref, state := range states {
		if !state.validPulls {
			continue
		}
		requiredPresubmits := sets.NewString()
		for _, pr := range state.prs {
			for _, job := range presubmits[int(pr.Number)] {
				requiredPresubmits.Insert(job.Context)
			}
		}
		overallState := successState
		for _, p := range requiredPresubmits.List() {
			if s, ok := state.jobStates[p]; !ok || s == failureState {
				overallState = failureState
				log.WithField("batch", ref).Debugf("batch invalid, required presubmit %s is not passing", p)
				break
			} else if s == pendingState && overallState == successState {
				overallState = pendingState
			}
		}
		switch overallState {
		// Currently we only consider 1 pending batch and 1 success batch at a time.
		// If more are somehow present they will be ignored.
		case pendingState:
			pendingBatch = state.prs
		case successState:
			successBatch = state.prs
		}
	}
	return successBatch, pendingBatch
}

// prHeadIsInPJPulls checks to see if the given pull request's head sha is one of the pull shas in the job.
func prHeadIsInPJPulls(pr PullRequest, pj plumber.PipelineOptions) bool {
	for _, pull := range pj.Spec.Refs.Pulls {
		if pull.SHA == string(pr.HeadRefOID) {
			return true
		}
	}
	return false
}

// accumulate returns the supplied PRs sorted into three buckets based on their
// accumulated state across the presubmits.
func accumulate(presubmits map[int][]config.Presubmit, prs []PullRequest, pjs []plumber.PipelineOptions, spc scmProviderClient, log *logrus.Entry) (successes, pendings, missings []PullRequest, missingTests map[int][]config.Presubmit) {

	missingTests = map[int][]config.Presubmit{}
	for _, pr := range prs {
		// Get the actual contexts for the HEAD of the PR as well, to deal with things like override.
		contexts, err := headContexts(log, spc, &pr)
		if err != nil {
			log.WithError(err).Error("Error getting head contexts, solely using PJs for status")
		}
		// Accumulate the best result for each job (Passing > Pending > Failing/Unknown)
		// We can ignore the baseSHA here because the subPool only contains PipelineActivitys with the correct baseSHA
		psStates := make(map[string]simpleState)
		for _, pj := range pjs {
			if pj.Spec.Type != plumber.PresubmitJob {
				continue
			}
			if len(pj.Spec.Refs.Pulls) == 0 || pj.Spec.Refs.Pulls[0].Number != int(pr.Number) {
				continue
			}
			if pj.Spec.Refs.Pulls[0].SHA != string(pr.HeadRefOID) {
				continue
			}

			name := pj.Spec.Context
			oldState := psStates[name]
			newState := toSimpleState(pj.Status.State)
			if oldState == failureState || oldState == "" {
				psStates[name] = newState
			} else if oldState == pendingState && newState == successState {
				psStates[name] = successState
			}
		}
		// Iterate over the commit status contexts
		for _, contextInfo := range contexts {
			// If there's already a state recorded, see if it needs to be updated due to override.
			if existingState, ok := psStates[string(contextInfo.Context)]; ok {
				psStates[string(contextInfo.Context)] = commitStatusOrPJStatusForContext(contextInfo, existingState)
			}
		}
		// The overall result for the PR is the worst of the best of all its
		// required Presubmits
		overallState := successState
		for _, ps := range presubmits[int(pr.Number)] {
			if s, ok := psStates[ps.Context]; !ok {
				// No PJ with correct baseSHA+headSHA exists
				missingTests[int(pr.Number)] = append(missingTests[int(pr.Number)], ps)
				log.WithFields(pr.logFields()).Debugf("missing presubmit %s", ps.Context)
			} else if s == failureState {
				// PJ with correct baseSHA+headSHA exists but failed
				missingTests[int(pr.Number)] = append(missingTests[int(pr.Number)], ps)
				log.WithFields(pr.logFields()).Debugf("presubmit %s not passing", ps.Context)
			} else if s == pendingState {
				log.WithFields(pr.logFields()).Debugf("presubmit %s pending", ps.Context)
				overallState = pendingState
			}
		}
		if len(missingTests[int(pr.Number)]) > 0 {
			overallState = failureState
		}

		if overallState == successState {
			successes = append(successes, pr)
		} else if overallState == pendingState {
			pendings = append(pendings, pr)
		} else {
			missings = append(missings, pr)
		}
	}
	return
}

func commitStatusOrPJStatusForContext(contextInfo Context, existingStatus simpleState) simpleState {
	// If the context status is success, the existing status is neither an empty string nor success, and the context
	// status description starts with util.OverriddenByPrefix, let's overwrite the state for the context.
	contextState := githubqlStatusStateToSimpleState(contextInfo.State)
	if existingStatus != "" && contextState == successState && existingStatus != successState &&
		strings.HasPrefix(string(contextInfo.Description), util.OverriddenByPrefix) {
		return contextState
	}
	return existingStatus
}

func prNumbers(prs []PullRequest) []int {
	var nums []int
	for _, pr := range prs {
		nums = append(nums, int(pr.Number))
	}
	return nums
}

func (c *DefaultController) pickBatch(sp subpool, cc contextChecker) ([]PullRequest, error) {
	batchLimit := c.config().Tide.BatchSizeLimit(sp.org, sp.repo)
	if batchLimit < 0 {
		sp.log.Debug("Batch merges disabled by configuration in this repo.")
		return nil, nil
	}
	// we must choose the oldest PRs for the batch
	sort.Slice(sp.prs, func(i, j int) bool { return sp.prs[i].Number < sp.prs[j].Number })

	var candidates []PullRequest
	for _, pr := range sp.prs {
		if isPassingTests(sp.log, c.spc, pr, cc) {
			candidates = append(candidates, pr)
		}
	}

	if len(candidates) == 0 {
		sp.log.Debugf("of %d possible PRs, none were passing tests, no batch will be created", len(sp.prs))
		return nil, nil
	}
	sp.log.Debugf("of %d possible PRs, %d are passing tests", len(sp.prs), len(candidates))

	r, err := c.gc.Clone(sp.org + "/" + sp.repo)
	if err != nil {
		return nil, err
	}
	defer r.Clean()
	if err := r.Config("user.name", "prow"); err != nil {
		return nil, err
	}
	if err := r.Config("user.email", "prow@localhost"); err != nil {
		return nil, err
	}
	if err := r.Config("commit.gpgsign", "false"); err != nil {
		sp.log.Warningf("Cannot set gpgsign=false in gitconfig: %v", err)
	}
	if err := r.Checkout(sp.sha); err != nil {
		return nil, err
	}

	var res []PullRequest
	for _, pr := range candidates {
		if ok, err := r.Merge(string(pr.HeadRefOID)); err != nil {
			// we failed to abort the merge and our git client is
			// in a bad state; it must be cleaned before we try again
			return nil, err
		} else if ok {
			res = append(res, pr)
			// TODO: Make this configurable per subpool.
			if batchLimit > 0 && len(res) >= batchLimit {
				break
			}
		}
	}
	return res, nil
}

func checkMergeLabels(pr PullRequest, squash, rebase, merge string, method gitprovider.PullRequestMergeType) (gitprovider.PullRequestMergeType, error) {
	labelCount := 0
	for _, prlabel := range pr.Labels.Nodes {
		switch string(prlabel.Name) {
		case squash:
			method = gitprovider.MergeSquash
			labelCount++
		case rebase:
			method = gitprovider.MergeRebase
			labelCount++
		case merge:
			method = gitprovider.MergeMerge
			labelCount++
		}
		if labelCount > 1 {
			return "", fmt.Errorf("conflicting merge method override labels")
		}
	}
	return method, nil
}

func (c *DefaultController) prepareMergeDetails(commitTemplates config.TideMergeCommitTemplate, pr PullRequest, mergeMethod gitprovider.PullRequestMergeType) gitprovider.MergeDetails {
	ghMergeDetails := gitprovider.MergeDetails{
		SHA:         string(pr.HeadRefOID),
		MergeMethod: string(mergeMethod),
	}

	if commitTemplates.Title != nil {
		var b bytes.Buffer

		if err := commitTemplates.Title.Execute(&b, pr); err != nil {
			c.logger.Errorf("error executing commit title template: %v", err)
		} else {
			ghMergeDetails.CommitTitle = b.String()
		}
	}

	if commitTemplates.Body != nil {
		var b bytes.Buffer

		if err := commitTemplates.Body.Execute(&b, pr); err != nil {
			c.logger.Errorf("error executing commit body template: %v", err)
		} else {
			ghMergeDetails.CommitMessage = b.String()
		}
	}

	return ghMergeDetails
}

func (c *DefaultController) mergePRs(sp subpool, prs []PullRequest) error {
	var merged, failed []int
	defer func() {
		if len(merged) == 0 {
			return
		}
		tideMetrics.merges.WithLabelValues(sp.org, sp.repo, sp.branch).Observe(float64(len(merged)))
	}()

	var errs []error
	log := sp.log.WithField("merge-targets", prNumbers(prs))
	for i, pr := range prs {
		log := log.WithFields(pr.logFields())
		mergeMethod := c.config().Tide.MergeMethod(sp.org, sp.repo)
		commitTemplates := c.config().Tide.MergeCommitTemplate(sp.org, sp.repo)
		squashLabel := c.config().Tide.SquashLabel
		rebaseLabel := c.config().Tide.RebaseLabel
		mergeLabel := c.config().Tide.MergeLabel
		if squashLabel != "" || rebaseLabel != "" || mergeLabel != "" {
			var err error
			mergeMethod, err = checkMergeLabels(pr, squashLabel, rebaseLabel, mergeLabel, mergeMethod)
			if err != nil {
				log.WithError(err).Error("Merge failed.")
				errs = append(errs, err)
				failed = append(failed, int(pr.Number))
				continue
			}
		}

		keepTrying, err := tryMerge(func() error {
			ghMergeDetails := c.prepareMergeDetails(commitTemplates, pr, mergeMethod)
			return c.spc.Merge(sp.org, sp.repo, int(pr.Number), ghMergeDetails)
		})
		if err != nil {
			log.WithError(err).Error("Merge failed.")
			errs = append(errs, err)
			failed = append(failed, int(pr.Number))
		} else {
			log.Info("Merged.")
			merged = append(merged, int(pr.Number))
		}
		if !keepTrying {
			break
		}
		// If we successfully merged this PR and have more to merge, sleep to give
		// GitHub time to recalculate mergeability.
		if err == nil && i+1 < len(prs) {
			sleep(time.Second * 5)
		}
	}

	if len(errs) == 0 {
		return nil
	}

	// Construct a more informative error.
	var batch string
	if len(prs) > 1 {
		batch = fmt.Sprintf(" from batch %v", prNumbers(prs))
		if len(merged) > 0 {
			batch = fmt.Sprintf("%s, partial merge %v", batch, merged)
		}
	}
	return fmt.Errorf("failed merging %v%s: %v", failed, batch, errorutil.NewAggregate(errs...))
}

// tryMerge attempts 1 merge and returns a bool indicating if we should try
// to merge the remaining PRs and possibly an error.
func tryMerge(mergeFunc func() error) (bool, error) {
	var err error
	const maxRetries = 3
	backoff := time.Second * 4
	for retry := 0; retry < maxRetries; retry++ {
		if err = mergeFunc(); err == nil {
			// Successful merge!
			return true, nil
		}
		// TODO: Add a config option to abort batches if a PR in the batch
		// cannot be merged for any reason. This would skip merging
		// not just the changed PR, but also the other PRs in the batch.
		// This shouldn't be the default behavior as merging batches is high
		// priority and this is unlikely to be problematic.
		// Note: We would also need to be able to roll back any merges for the
		// batch that were already successfully completed before the failure.
		// Ref: https://github.com/kubernetes/test-infra/issues/10621
		if _, ok := err.(gitprovider.ModifiedHeadError); ok {
			// This is a possible source of incorrect behavior. If someone
			// modifies their PR as we try to merge it in a batch then we
			// end up in an untested state. This is unlikely to cause any
			// real problems.
			return true, fmt.Errorf("PR was modified: %v", err)
		} else if _, ok = err.(gitprovider.UnmergablePRBaseChangedError); ok {
			//  complained that the base branch was modified. This is a
			// strange error because the API doesn't even allow the request to
			// specify the base branch sha, only the head sha.
			// We suspect that github is complaining because we are making the
			// merge requests too rapidly and it cannot recompute mergability
			// in time. https://gitprovider.com/kubernetes/test-infra/issues/5171
			// We handle this by sleeping for a few seconds before trying to
			// merge again.
			err = fmt.Errorf("base branch was modified: %v", err)
			if retry+1 < maxRetries {
				sleep(backoff)
				backoff *= 2
			}
		} else if _, ok = err.(gitprovider.UnauthorizedToPushError); ok {
			// GitHub let us know that the token used cannot push to the branch.
			// Even if the robot is set up to have write access to the repo, an
			// overzealous branch protection setting will not allow the robot to
			// push to a specific branch.
			// We won't be able to merge the other PRs.
			return false, fmt.Errorf("branch needs to be configured to allow this robot to push: %v", err)
		} else if _, ok = err.(gitprovider.MergeCommitsForbiddenError); ok {
			// GitHub let us know that the merge method configured for this repo
			// is not allowed by other repo settings, so we should let the admins
			// know that the configuration needs to be updated.
			// We won't be able to merge the other PRs.
			return false, fmt.Errorf("Tide needs to be configured to use the 'rebase' merge method for this repo or the repo needs to allow merge commits: %v", err)
		} else if _, ok = err.(gitprovider.UnmergablePRError); ok {
			return true, fmt.Errorf("PR is unmergable. Do the Tide merge requirements match the GitHub settings for the repo? %v", err)
		}
		return true, err
	}
	// We ran out of retries. Return the last transient error.
	return true, err
}

func (c *DefaultController) trigger(sp subpool, presubmits map[int][]config.Presubmit, prs []PullRequest) error {
	refs := plumber.Refs{
		Org:     sp.org,
		Repo:    sp.repo,
		BaseRef: sp.branch,
		BaseSHA: sp.sha,
	}
	for _, pr := range prs {
		refs.Pulls = append(
			refs.Pulls,
			plumber.Pull{
				Number: int(pr.Number),
				Author: string(pr.Author.Login),
				SHA:    string(pr.HeadRefOID),
			},
		)
	}

	// If PRs require the same job, we only want to trigger it once.
	// If multiple required jobs have the same context, we assume the
	// same shard will be run to provide those contexts
	triggeredContexts := sets.NewString()
	for _, pr := range prs {
		for _, ps := range presubmits[int(pr.Number)] {
			if triggeredContexts.Has(string(ps.Context)) {
				continue
			}
			triggeredContexts.Insert(string(ps.Context))
			var spec plumber.PipelineOptionsSpec
			if len(prs) == 1 {
				spec = pjutil.PresubmitSpec(ps, refs)
			} else {
				spec = pjutil.BatchSpec(ps, refs)
			}
			pj := pjutil.NewPlumberJob(spec, ps.Labels, ps.Annotations)
			start := time.Now()
			cloneURL := string(pr.Repository.URL)
			if cloneURL == "" {
				c.logger.WithField("owner", refs.Org).WithField("repository", refs.Repo).Warnf("no GitURL returned")
				// TODO load URL via scm client?
			}
			if !strings.HasPrefix(cloneURL, ".git") {
				cloneURL = cloneURL + ".git"
			}
			repo := scm.Repository{
				Name:      string(pr.Repository.Name),
				Namespace: string(pr.Repository.Owner.Login),
				Branch:    string(pr.BaseRef.Name),
				Clone:     cloneURL,
			}
			if _, err := c.plumberClient.Create(&pj, c.mpClient, repo); err != nil {
				c.logger.WithField("duration", time.Since(start).String()).Debug("Failed to create pipeline on the cluster.")
				return fmt.Errorf("failed to create a pipeline for job: %q, PRs: %v: %v", spec.Job, prNumbers(prs), err)
			}
			sha := refs.BaseSHA
			if len(refs.Pulls) > 0 {
				sha = refs.Pulls[0].SHA
			}

			statusInput := &scm.StatusInput{
				State: scm.StatePending,
				Label: spec.Context,
				Desc:  util.CommitStatusPendingDescription,
			}
			if _, err := c.spc.CreateStatus(refs.Org, refs.Repo, sha, statusInput); err != nil {
				c.logger.WithField("duration", time.Since(start).String()).Debug("Failed to set pending status on triggered context.")
				return errors.Wrapf(err, "Cannot update PR status on org %s repo %s sha %s for context %s", refs.Org, refs.Repo, sha, statusInput.Label)
			}
			c.logger.WithField("duration", time.Since(start).String()).Debug("Created pipeline on the cluster.")
		}
	}
	return nil
}

func (c *DefaultController) takeAction(sp subpool, batchPending, successes, pendings, missings, batchMerges []PullRequest, missingSerialTests map[int][]config.Presubmit) (Action, []PullRequest, error) {
	// Merge the batch!
	if len(batchMerges) > 0 {
		return MergeBatch, batchMerges, c.mergePRs(sp, batchMerges)
	}
	// Do not merge PRs while waiting for a batch to complete. We don't want to
	// invalidate the old batch result.
	if len(successes) > 0 && len(batchPending) == 0 {
		if ok, pr := pickSmallestPassingNumber(sp.log, c.spc, successes, sp.cc); ok {
			return Merge, []PullRequest{pr}, c.mergePRs(sp, []PullRequest{pr})
		}
	}
	// If no presubmits are configured, just wait.
	if len(sp.presubmits) == 0 {
		return Wait, nil, nil
	}
	// If we have no batch, trigger one.
	if len(sp.prs) > 1 && len(batchPending) == 0 {
		batch, err := c.pickBatch(sp, sp.cc)
		if err != nil {
			return Wait, nil, err
		}
		if len(batch) > 1 {
			return TriggerBatch, batch, c.trigger(sp, sp.presubmits, batch)
		}
	}
	// If we have no serial jobs pending or successful, trigger one.
	if len(missings) > 0 && len(pendings) == 0 && len(successes) == 0 {
		if ok, pr := pickSmallestPassingNumber(sp.log, c.spc, missings, sp.cc); ok {
			return Trigger, []PullRequest{pr}, c.trigger(sp, missingSerialTests, []PullRequest{pr})
		}
	}
	return Wait, nil, nil
}

// changedFilesAgent queries and caches the names of files changed by PRs.
// Cache entries expire if they are not used during a sync loop.
type changedFilesAgent struct {
	spc         scmProviderClient
	changeCache map[changeCacheKey][]string
	// nextChangeCache caches file change info that is relevant this sync for use next sync.
	// This becomes the new changeCache when prune() is called at the end of each sync.
	nextChangeCache map[changeCacheKey][]string
	sync.RWMutex
}

type changeCacheKey struct {
	org, repo string
	number    int
	sha       string
}

// prChanges gets the files changed by the PR, either from the cache or by
// querying GitHub.
func (c *changedFilesAgent) prChanges(pr *PullRequest) config.ChangedFilesProvider {
	return func() ([]string, error) {
		cacheKey := changeCacheKey{
			org:    string(pr.Repository.Owner.Login),
			repo:   string(pr.Repository.Name),
			number: int(pr.Number),
			sha:    string(pr.HeadRefOID),
		}

		c.RLock()
		changedFiles, ok := c.changeCache[cacheKey]
		if ok {
			c.RUnlock()
			c.Lock()
			c.nextChangeCache[cacheKey] = changedFiles
			c.Unlock()
			return changedFiles, nil
		}
		if changedFiles, ok = c.nextChangeCache[cacheKey]; ok {
			c.RUnlock()
			return changedFiles, nil
		}
		c.RUnlock()

		// We need to query the changes from GitHub.
		changes, err := c.spc.GetPullRequestChanges(
			string(pr.Repository.Owner.Login),
			string(pr.Repository.Name),
			int(pr.Number),
		)
		if err != nil {
			return nil, fmt.Errorf("error getting PR changes for #%d: %v", int(pr.Number), err)
		}
		changedFiles = make([]string, 0, len(changes))
		for _, change := range changes {
			changedFiles = append(changedFiles, change.Path)
		}

		c.Lock()
		c.nextChangeCache[cacheKey] = changedFiles
		c.Unlock()
		return changedFiles, nil
	}
}

// prune removes any cached file changes that were not used since the last prune.
func (c *changedFilesAgent) prune() {
	c.Lock()
	defer c.Unlock()
	c.changeCache = c.nextChangeCache
	c.nextChangeCache = make(map[changeCacheKey][]string)
}

func (c *DefaultController) presubmitsByPull(sp *subpool) (map[int][]config.Presubmit, error) {
	presubmits := make(map[int][]config.Presubmit, len(sp.prs))
	record := func(num int, job config.Presubmit) {
		if jobs, ok := presubmits[num]; ok {
			presubmits[num] = append(jobs, job)
		} else {
			presubmits[num] = []config.Presubmit{job}
		}
	}

	for _, ps := range c.config().Presubmits[sp.org+"/"+sp.repo] {
		if !ps.ContextRequired() {
			continue
		}

		for _, pr := range sp.prs {
			if shouldRun, err := ps.ShouldRun(sp.branch, c.changedFiles.prChanges(&pr), false, false); err != nil {
				return nil, err
			} else if shouldRun {
				record(int(pr.Number), ps)
			}
		}
	}
	return presubmits, nil
}

func (c *DefaultController) syncSubpool(sp subpool, blocks []blockers.Blocker) (Pool, error) {
	sp.log.Infof("Syncing subpool: %d PRs, %d PJs.", len(sp.prs), len(sp.pjs))
	successes, pendings, missings, missingSerialTests := accumulate(sp.presubmits, sp.prs, sp.pjs, c.spc, sp.log)
	batchMerge, batchPending := accumulateBatch(sp.presubmits, sp.prs, sp.pjs, c.spc, sp.log)
	sp.log.WithFields(logrus.Fields{
		"prs-passing":   prNumbers(successes),
		"prs-pending":   prNumbers(pendings),
		"prs-missing":   prNumbers(missings),
		"batch-passing": prNumbers(batchMerge),
		"batch-pending": prNumbers(batchPending),
	}).Info("Subpool accumulated.")

	var act Action
	var targets []PullRequest
	var err error
	var errorString string
	if len(blocks) > 0 {
		act = PoolBlocked
	} else {
		act, targets, err = c.takeAction(sp, batchPending, successes, pendings, missings, batchMerge, missingSerialTests)
		if err != nil {
			errorString = err.Error()
		}
		if recordableActions[act] {
			c.History.Record(
				poolKey(sp.org, sp.repo, sp.branch),
				string(act),
				sp.sha,
				errorString,
				prMeta(targets...),
			)
		}
	}

	sp.log.WithFields(logrus.Fields{
		"action":  string(act),
		"targets": prNumbers(targets),
	}).Info("Subpool synced.")
	tideMetrics.pooledPRs.WithLabelValues(sp.org, sp.repo, sp.branch).Set(float64(len(sp.prs)))
	tideMetrics.updateTime.WithLabelValues(sp.org, sp.repo, sp.branch).Set(float64(time.Now().Unix()))
	return Pool{
			Org:    sp.org,
			Repo:   sp.repo,
			Branch: sp.branch,

			SuccessPRs: successes,
			PendingPRs: pendings,
			MissingPRs: missings,

			BatchPending: batchPending,

			Action:   act,
			Target:   targets,
			Blockers: blocks,
			Error:    errorString,
		},
		err
}

func prMeta(prs ...PullRequest) []plumber.Pull {
	var res []plumber.Pull
	for _, pr := range prs {
		res = append(res, plumber.Pull{
			Number: int(pr.Number),
			Author: string(pr.Author.Login),
			Title:  string(pr.Title),
			SHA:    string(pr.HeadRefOID),
		})
	}
	return res
}

func sortPools(pools []Pool) {
	sort.Slice(pools, func(i, j int) bool {
		if string(pools[i].Org) != string(pools[j].Org) {
			return string(pools[i].Org) < string(pools[j].Org)
		}
		if string(pools[i].Repo) != string(pools[j].Repo) {
			return string(pools[i].Repo) < string(pools[j].Repo)
		}
		return string(pools[i].Branch) < string(pools[j].Branch)
	})

	sortPRs := func(prs []PullRequest) {
		sort.Slice(prs, func(i, j int) bool { return int(prs[i].Number) < int(prs[j].Number) })
	}
	for i := range pools {
		sortPRs(pools[i].SuccessPRs)
		sortPRs(pools[i].PendingPRs)
		sortPRs(pools[i].MissingPRs)
		sortPRs(pools[i].BatchPending)
	}
}

type subpool struct {
	log    *logrus.Entry
	org    string
	repo   string
	branch string
	// sha is the baseSHA for this subpool
	sha string

	// pjs contains all PipelineActivitys of type Presubmit or Batch
	// that have the same baseSHA as the subpool
	pjs []plumber.PipelineOptions
	prs []PullRequest

	cc contextChecker
	// presubmit contains all required presubmits for each PR
	// in this subpool
	presubmits map[int][]config.Presubmit
}

func poolKey(org, repo, branch string) string {
	return fmt.Sprintf("%s/%s:%s", org, repo, branch)
}

// dividePool splits up the list of pull requests and prow jobs into a group
// per repo and branch. It only keeps PipelineActivitys that match the latest branch.
func (c *DefaultController) dividePool(pool map[string]PullRequest, pjs []plumber.PipelineOptions) (map[string]*subpool, error) {
	sps := make(map[string]*subpool)
	for _, pr := range pool {
		org := string(pr.Repository.Owner.Login)
		repo := string(pr.Repository.Name)
		branch := string(pr.BaseRef.Name)
		branchRef := string(pr.BaseRef.Prefix) + string(pr.BaseRef.Name)
		fn := poolKey(org, repo, branch)
		if sps[fn] == nil {
			sha, err := c.spc.GetRef(org, repo, strings.TrimPrefix(branchRef, "refs/"))
			if err != nil {
				return nil, err
			}
			sps[fn] = &subpool{
				log: c.logger.WithFields(logrus.Fields{
					"org":      org,
					"repo":     repo,
					"branch":   branch,
					"base-sha": sha,
				}),
				org:    org,
				repo:   repo,
				branch: branch,
				sha:    sha,
			}
		}
		sps[fn].prs = append(sps[fn].prs, pr)
	}
	for _, pj := range pjs {
		if pj.Spec.Type != plumber.PresubmitJob && pj.Spec.Type != plumber.BatchJob {
			continue
		}
		fn := poolKey(pj.Spec.Refs.Org, pj.Spec.Refs.Repo, pj.Spec.Refs.BaseRef)
		if sps[fn] == nil || pj.Spec.Refs.BaseSHA != sps[fn].sha {
			continue
		}
		sps[fn].pjs = append(sps[fn].pjs, pj)
	}
	return sps, nil
}

// PullRequest holds graphql data about a PR, including its commits and their contexts.
type PullRequest struct {
	Number githubql.Int
	Author struct {
		Login githubql.String
	}
	BaseRef struct {
		Name   githubql.String
		Prefix githubql.String
	}
	HeadRefName githubql.String `graphql:"headRefName"`
	HeadRefOID  githubql.String `graphql:"headRefOid"`
	Mergeable   githubql.MergeableState
	Repository  struct {
		Name          githubql.String
		NameWithOwner githubql.String
		URL           githubql.String
		Owner         struct {
			Login githubql.String
		}
	}
	Commits struct {
		Nodes []struct {
			Commit Commit
		}
		// Request the 'last' 4 commits hoping that one of them is the logically 'last'
		// commit with OID matching HeadRefOID. If we don't find it we have to use an
		// additional API token. (see the 'headContexts' func for details)
		// We can't raise this too much or we could hit the limit of 50,000 nodes
		// per query: https://developer.github.com/v4/guides/resource-limitations/#node-limit
	} `graphql:"commits(last: 4)"`
	Labels struct {
		Nodes []struct {
			Name githubql.String
		}
	} `graphql:"labels(first: 100)"`
	Milestone *struct {
		Title githubql.String
	}
	Body      githubql.String
	Title     githubql.String
	UpdatedAt githubql.DateTime
}

// Commit holds graphql data about commits and which contexts they have
type Commit struct {
	Status struct {
		Contexts []Context
	}
	OID githubql.String `graphql:"oid"`
}

// Context holds graphql response data for github contexts.
type Context struct {
	Context     githubql.String
	Description githubql.String
	State       githubql.StatusState
}

// PRNode a node containing a PR
type PRNode struct {
	PullRequest PullRequest `graphql:"... on PullRequest"`
}

type searchQuery struct {
	RateLimit struct {
		Cost      githubql.Int
		Remaining githubql.Int
	}
	Search struct {
		PageInfo struct {
			HasNextPage githubql.Boolean
			EndCursor   githubql.String
		}
		Nodes []PRNode
	} `graphql:"search(type: ISSUE, first: 100, after: $searchCursor, query: $query)"`
}

func (pr *PullRequest) logFields() logrus.Fields {
	return logrus.Fields{
		"org":  string(pr.Repository.Owner.Login),
		"repo": string(pr.Repository.Name),
		"pr":   int(pr.Number),
		"sha":  string(pr.HeadRefOID),
	}
}

// headContexts gets the status contexts for the commit with OID == pr.HeadRefOID
//
// First, we try to get this value from the commits we got with the PR query.
// Unfortunately the 'last' commit ordering is determined by author date
// not commit date so if commits are reordered non-chronologically on the PR
// branch the 'last' commit isn't necessarily the logically last commit.
// We list multiple commits with the query to increase our chance of success,
// but if we don't find the head commit we have to ask GitHub for it
// specifically (this costs an API token).
func headContexts(log *logrus.Entry, spc scmProviderClient, pr *PullRequest) ([]Context, error) {
	for _, node := range pr.Commits.Nodes {
		if node.Commit.OID == pr.HeadRefOID {
			return node.Commit.Status.Contexts, nil
		}
	}
	// We didn't get the head commit from the query (the commits must not be
	// logically ordered) so we need to specifically ask GitHub for the status
	// and coerce it to a graphql type.
	org := string(pr.Repository.Owner.Login)
	repo := string(pr.Repository.Name)
	// Log this event so we can tune the number of commits we list to minimize this.
	log.Warnf("'last' %d commits didn't contain logical last commit. Querying GitHub...", len(pr.Commits.Nodes))
	combined, err := spc.GetCombinedStatus(org, repo, string(pr.HeadRefOID))
	if err != nil {
		return nil, fmt.Errorf("failed to get the combined status: %v", err)
	}
	contexts := make([]Context, 0, len(combined.Statuses))
	for _, status := range combined.Statuses {
		contexts = append(
			contexts,
			Context{
				Context:     githubql.String(status.Label),
				Description: githubql.String(status.Desc),
				State:       githubql.StatusState(strings.ToUpper(status.State.String())),
			},
		)
	}
	// Add a commit with these contexts to pr for future look ups.
	pr.Commits.Nodes = append(pr.Commits.Nodes,
		struct{ Commit Commit }{
			Commit: Commit{
				OID:    pr.HeadRefOID,
				Status: struct{ Contexts []Context }{Contexts: contexts},
			},
		},
	)
	return contexts, nil
}

func orgRepoQueryString(orgs, repos []string, orgExceptions map[string]sets.String) string {
	toks := make([]string, 0, len(orgs))
	for _, o := range orgs {
		toks = append(toks, fmt.Sprintf("org:\"%s\"", o))

		for _, e := range orgExceptions[o].List() {
			toks = append(toks, fmt.Sprintf("-repo:\"%s\"", e))
		}
	}
	for _, r := range repos {
		toks = append(toks, fmt.Sprintf("repo:\"%s\"", r))
	}
	return strings.Join(toks, " ")
}
