/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	log "github.com/golang/glog"
	mesos "github.com/mesos/mesos-go/mesosproto"
	util "github.com/mesos/mesos-go/mesosutil"
	"github.com/mesos/mesos-go/scheduler"
	"github.com/samuel/go-zookeeper/zk"

	"github.com/mesosphere/etcd-mesos/config"
	"github.com/mesosphere/etcd-mesos/offercache"
	"github.com/mesosphere/etcd-mesos/rpc"
)

const (
	portsPerTask   = 3
	notReseeding   = 0
	reseedUnderway = 1
)

// State represents the mutability of the scheduler.
type State int32

const (
	// Mutable scheduler state occurs during:
	// * starting up for the first time
	// * growing (recovering) from 1 to N nodes
	// * pruning dead nodes
	// * exiting
	Mutable State = iota
	// Immutable scheduler state occurs during:
	// * waiting for state to settle during initialization
	// * disconnection from the Mesos master
	// * performing a backup with the intention of seeding a new cluster
	Immutable
)

type EtcdScheduler struct {
	Stats                  Stats
	Master                 string
	ExecutorPath           string
	EtcdPath               string
	ClusterName            string
	ZkConnect              string
	ZkChroot               string
	ZkServers              []string
	singleInstancePerSlave bool
	desiredInstanceCount   int
	healthCheck            func(map[string]*config.Node) error
	shutdown               func()
	stateFunc              func(string) (*rpc.MasterState, error)
	mut                    sync.RWMutex
	state                  State
	frameworkID            *mesos.FrameworkID
	masterInfo             *mesos.MasterInfo
	pending                map[string]struct{}
	running                map[string]*config.Node
	tasks                  map[string]*mesos.TaskID
	highestInstanceID      int64
	executorUris           []*mesos.CommandInfo_URI
	offerCache             *offercache.OfferCache
	launchChan             chan struct{}
	diskPerTask            float64
	cpusPerTask            float64
	memPerTask             float64
	pauseChan              chan struct{}
	chillSeconds           time.Duration
	autoReseedEnabled      bool
	reseedTimeout          time.Duration
	livelockWindow         *time.Time
	reseeding              int32
}

type Stats struct {
	RunningServers   uint32 `json:"running_servers"`
	LaunchedServers  uint32 `json:"launched_servers"`
	FailedServers    uint32 `json:"failed_servers"`
	ClusterLivelocks uint32 `json:"cluster_livelocks"`
	ClusterReseeds   uint32 `json:"cluster_reseeds"`
	IsHealthy        uint32 `json:"healthy"`
}

type OfferResources struct {
	cpus  float64
	mems  float64
	disk  float64
	ports []*mesos.Value_Range
}

func NewEtcdScheduler(
	desiredInstanceCount int,
	chillSeconds int,
	reseedTimeout int,
	autoReseed bool,
	executorUris []*mesos.CommandInfo_URI,
	singleInstancePerSlave bool,
	diskPerTask float64,
	cpusPerTask float64,
	memPerTask float64,
) *EtcdScheduler {
	return &EtcdScheduler{
		state:                Immutable,
		running:              map[string]*config.Node{},
		pending:              map[string]struct{}{},
		tasks:                map[string]*mesos.TaskID{},
		highestInstanceID:    time.Now().Unix(),
		executorUris:         executorUris,
		ZkServers:            []string{},
		chillSeconds:         time.Duration(chillSeconds),
		autoReseedEnabled:    autoReseed,
		reseedTimeout:        time.Second * time.Duration(reseedTimeout),
		desiredInstanceCount: desiredInstanceCount,
		launchChan:           make(chan struct{}, 2048),
		pauseChan:            make(chan struct{}, 2048),
		offerCache: offercache.New(
			desiredInstanceCount,
			singleInstancePerSlave,
		),
		healthCheck:            rpc.HealthCheck,
		shutdown:               func() { os.Exit(1) },
		stateFunc:              rpc.GetState,
		singleInstancePerSlave: singleInstancePerSlave,
		diskPerTask:            diskPerTask,
		cpusPerTask:            cpusPerTask,
		memPerTask:             memPerTask,
	}
}

// ----------------------- mesos callbacks ------------------------- //

func (s *EtcdScheduler) Registered(
	driver scheduler.SchedulerDriver,
	frameworkID *mesos.FrameworkID,
	masterInfo *mesos.MasterInfo,
) {
	log.Infoln("Framework Registered with Master ", masterInfo)
	s.mut.Lock()
	s.frameworkID = frameworkID
	s.mut.Unlock()

	if s.ZkConnect != "" {
		err := rpc.PersistFrameworkID(
			frameworkID,
			s.ZkServers,
			s.ZkChroot,
			s.ClusterName,
		)
		if err != nil && err != zk.ErrNodeExists {
			log.Errorf("Failed to persist framework ID: %s", err)
			if s.shutdown != nil {
				s.shutdown()
			}
		} else if err == zk.ErrNodeExists {
			log.Warning("Framework ID is already persisted for this cluster.")
		}
	}

	s.Initialize(driver, masterInfo)
}

func (s *EtcdScheduler) Reregistered(
	driver scheduler.SchedulerDriver,
	masterInfo *mesos.MasterInfo,
) {
	log.Infoln("Framework Reregistered with Master ", masterInfo)
	s.Initialize(driver, masterInfo)
}

func (s *EtcdScheduler) Disconnected(scheduler.SchedulerDriver) {
	log.Error("Mesos master disconnected.")
	s.mut.Lock()
	s.state = Immutable
	s.mut.Unlock()
}

func (s *EtcdScheduler) ResourceOffers(
	driver scheduler.SchedulerDriver,
	offers []*mesos.Offer,
) {
	for _, offer := range offers {
		resources := parseOffer(offer)

		totalPorts := uint64(0)
		for _, pr := range resources.ports {
			totalPorts += (*pr.End + 1) - *pr.Begin
		}

		log.V(2).Infoln("Received Offer <", offer.Id.GetValue(),
			"> with cpus=", resources.cpus,
			" mem=", resources.mems,
			" ports=", totalPorts,
			" disk=", resources.disk,
			" from slave ", *offer.SlaveId.Value)

		s.mut.RLock()
		if s.state == Immutable {
			log.V(2).Info("Scheduler is Immutable.  Declining received offer.")
			s.decline(driver, offer)
			s.mut.RUnlock()
			continue
		}
		s.mut.RUnlock()

		alreadyUsingSlave := false
		for _, config := range s.RunningCopy() {
			if config.SlaveID == offer.GetSlaveId().GetValue() {
				alreadyUsingSlave = true
				break
			}
		}
		if alreadyUsingSlave {
			log.V(2).Infoln("Already using this slave for etcd instance.")
			if s.singleInstancePerSlave {
				log.V(2).Infoln("Skipping offer.")
				s.decline(driver, offer)
				continue
			}
			log.V(2).Infoln("-single-instance-per-slave is false, continuing.")
		}

		if resources.cpus < s.cpusPerTask {
			log.Infoln("Offer cpu is insufficient.")
		}

		if resources.mems < s.memPerTask {
			log.Infoln("Offer memory is insufficient.")
		}

		if totalPorts < portsPerTask {
			log.Infoln("Offer ports are insuffient.")
		}

		if resources.disk < s.diskPerTask {
			log.Infoln("Offer disk is insufficient.")
		}

		if resources.cpus >= s.cpusPerTask &&
			resources.mems >= s.memPerTask &&
			totalPorts >= portsPerTask &&
			resources.disk >= s.diskPerTask &&
			s.offerCache.Push(offer) {

			// golang for-loop variable reuse necessitates a copy here.
			offerCpy := *offer
			go func() {
				time.Sleep(s.chillSeconds / 2 * time.Second)
				// Decline the offer if we don't try to take it after a few seconds.
				if s.offerCache.Rescind(offerCpy.Id) {
					s.decline(driver, &offerCpy)
				}
			}()

			log.V(2).Infoln("Added offer to offer cache.")
			s.QueueLaunchAttempt()
		} else {
			s.decline(driver, offer)
			log.V(2).Infoln("Offer rejected.")
		}
	}
}

func (s *EtcdScheduler) StatusUpdate(
	driver scheduler.SchedulerDriver,
	status *mesos.TaskStatus,
) {
	log.Infoln(
		"Status update: task",
		status.TaskId.GetValue(),
		" is in state ",
		status.State.Enum().String(),
	)

	s.mut.Lock()
	defer s.mut.Unlock()

	node, err := config.Parse(status.GetTaskId().GetValue())
	if err != nil {
		log.Errorf("scheduler: failed to unmarshal config.Node from TaskId: %s", err)
		return
	}
	node.SlaveID = status.SlaveId.GetValue()

	// If this task was pending, we now know it's running or dead.
	delete(s.pending, node.Name)

	switch status.GetState() {
	case mesos.TaskState_TASK_LOST,
		mesos.TaskState_TASK_FINISHED,
		mesos.TaskState_TASK_KILLED,
		mesos.TaskState_TASK_ERROR,
		mesos.TaskState_TASK_FAILED:
		atomic.AddUint32(&s.Stats.FailedServers, 1)
		// Pump the brakes so that we have time to deconfigure the lost node
		// before adding a new one.  If we don't deconfigure first, we risk
		// split brain.
		s.PumpTheBrakes()
		delete(s.running, node.Name)
		delete(s.tasks, node.Name)
		s.QueueLaunchAttempt()
	case mesos.TaskState_TASK_RUNNING:
		_, present := s.running[node.Name]
		if !present {
			s.running[node.Name] = node
			s.tasks[node.Name] = status.TaskId
		}

		// During reconcilliation, we may find nodes with higher ID's due to ntp drift
		etcdIndexParts := strings.Split(node.Name, "-")
		if len(etcdIndexParts) != 2 {
			log.Warning("Task has a Name that does not follow the form etcd-<index>")
		} else {
			etcdIndex, err := strconv.ParseInt(etcdIndexParts[1], 10, 64)
			if err != nil {
				log.Warning("Task has a Name that does not follow the form etcd-<index>")
			} else {
				if etcdIndex > s.highestInstanceID {
					s.highestInstanceID = etcdIndex + 1
				}
			}
		}
	default:
		log.Warningf("Received unhandled task state: %+v", status.GetState())
	}

	if len(s.running) == 0 {
		log.Error("TOTAL CLUSTER LOSS!  LOCKING SCHEDULER, FOLLOW RESTORATION GUIDE AT https://github.com/mesosphere/etcd-mesos/blob/master/docs/response.md")
		s.state = Immutable
	}
}

func (s *EtcdScheduler) OfferRescinded(
	driver scheduler.SchedulerDriver,
	offerID *mesos.OfferID,
) {
	log.Info("received OfferRescinded rpc")
	s.offerCache.Rescind(offerID)
}

func (s *EtcdScheduler) FrameworkMessage(
	driver scheduler.SchedulerDriver,
	exec *mesos.ExecutorID,
	slave *mesos.SlaveID,
	msg string,
) {
	log.Info("received framework message: %s", msg)
}
func (s *EtcdScheduler) SlaveLost(
	scheduler.SchedulerDriver,
	*mesos.SlaveID,
) {
	log.Info("received slave lost rpc")
}
func (s *EtcdScheduler) ExecutorLost(
	scheduler.SchedulerDriver,
	*mesos.ExecutorID,
	*mesos.SlaveID,
	int,
) {
	log.Info("received executor lost rpc")
}

func (s *EtcdScheduler) Error(driver scheduler.SchedulerDriver, err string) {
	log.Infoln("Scheduler received error:", err)
	if err == "Completed framework attempted to re-register" {
		rpc.ClearZKState(s.ZkServers, s.ZkChroot, s.ClusterName)
		log.Error(
			"Removing reference to completed " +
				"framework in zookeeper and dying.",
		)
		if s.shutdown != nil {
			s.shutdown()
		}
	}
}

// ----------------------- helper functions ------------------------- //

// decline declines an offer.
func (s *EtcdScheduler) decline(
	driver scheduler.SchedulerDriver,
	offer *mesos.Offer,
) {
	log.V(2).Infof("Declining offer %s.", offer.Id.GetValue())
	driver.DeclineOffer(
		offer.Id,
		&mesos.Filters{
			RefuseSeconds: proto.Float64(float64(5 * s.chillSeconds)),
		},
	)
}

// RunningCopy makes a copy of the running map to minimize time
// spent with the scheduler lock is minimized.
func (s *EtcdScheduler) RunningCopy() map[string]*config.Node {
	s.mut.RLock()
	defer s.mut.RUnlock()
	runningCopy := map[string]*config.Node{}
	for k, v := range s.running {
		runningCopy[k] = v
	}
	return runningCopy
}

func (s *EtcdScheduler) Initialize(
	driver scheduler.SchedulerDriver,
	masterInfo *mesos.MasterInfo,
) {
	// Reset mutable state
	s.mut.Lock()
	s.running = map[string]*config.Node{}
	s.masterInfo = masterInfo
	s.mut.Unlock()

	go s.attemptMasterSync(driver)
}

func (s *EtcdScheduler) attemptMasterSync(driver scheduler.SchedulerDriver) {
	// Request that the master send us TaskStatus for live tasks.
	backoff := 1
	for retries := 0; retries < 5; retries++ {
		_, err := driver.ReconcileTasks([]*mesos.TaskStatus{})
		if err != nil {
			log.Errorf("Error while calling ReconcileTasks: %s", err)
		} else {
			// We want to allow some time for reconciled updates to arrive.
			err := s.waitForMasterSync()
			if err != nil {
				log.Error(err)
			} else {
				s.mut.Lock()
				log.Info("Scheduler transitioning to Mutable state.")
				s.state = Mutable
				s.mut.Unlock()
				return
			}
		}
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = int(math.Min(float64(backoff<<1), 8))
	}
	log.Error("Failed to synchronize with master!  " +
		"It is dangerous to continue at this point.  Dying.")
	if s.shutdown != nil {
		s.shutdown()
	}

}

func (s *EtcdScheduler) isInSync(masterState *rpc.MasterState) bool {
	peers, err := rpc.GetPeersFromState(masterState, s.ClusterName)
	if err != nil {
		log.Errorf("Could not get peers from master state: %v", err)
		return false
	}
	s.mut.RLock()
	defer s.mut.RUnlock()
	if len(peers) == len(s.running) {
		return true
	}
	return false
}

func (s *EtcdScheduler) waitForMasterSync() error {
	backoff := 1
	for retries := 0; retries < 5; retries++ {
		log.Info("Trying to sync with master.")

		if s.masterInfo == nil || s.masterInfo.Hostname == nil {
			return errors.New("No master info.")
		}
		s.mut.RLock()
		host := *s.masterInfo.Hostname
		port := strconv.Itoa(int(*s.masterInfo.Port))
		s.mut.RUnlock()

		if s.stateFunc == nil {
			return errors.New("No state function.")
		}
		masterState, err := s.stateFunc("http://" + host + ":" + port)
		if err != nil {
			log.Errorf("Unable to get master state.json: %v", err)
		} else {
			if s.isInSync(masterState) {
				log.Info("Scheduler synchronized with master.")
				return nil
			} else {
				log.Warning("Scheduler not yet in sync with master.")
			}
		}
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = int(math.Min(float64(backoff<<1), 8))
	}
	return errors.New("Unable to sync with master.")
}

func (s *EtcdScheduler) QueueLaunchAttempt() {
	select {
	case s.launchChan <- struct{}{}:
	default:
		// Somehow launchChan is full...
		log.Warning("launchChan is full!")
	}
}

func (s *EtcdScheduler) PumpTheBrakes() {
	select {
	case s.pauseChan <- struct{}{}:
	default:
		log.Warning("pauseChan is full!")
	}
}

func (s *EtcdScheduler) PeriodicLaunchRequestor() {
	for {
		s.mut.RLock()
		log.Infof(
			"running instances: %d desired: %d offers: %d",
			len(s.running), s.desiredInstanceCount, s.offerCache.Len(),
		)
		atomic.StoreUint32(&s.Stats.RunningServers, uint32(len(s.running)))

		if len(s.running) < s.desiredInstanceCount &&
			s.state == Mutable {
			s.QueueLaunchAttempt()
		} else if s.state == Immutable {
			log.Info("PeriodicLaunchRequestor skipping due to " +
				"Immutable scheduler state.")
		}
		s.mut.RUnlock()
		time.Sleep(5 * s.chillSeconds * time.Second)
	}
}

func (s *EtcdScheduler) Prune() error {
	s.mut.RLock()
	defer s.mut.RUnlock()
	if s.state == Mutable {
		configuredMembers, err := rpc.MemberList(s.running)
		if err != nil {
			log.Errorf("Prune could not retrieve current member list: %s",
				err)
			return err
		} else {
			for k := range configuredMembers {
				_, present := s.running[k]
				if !present {
					log.Warningf("Prune attempting to deconfigure unknown etcd "+
						"instance: %s", k)
					if err := rpc.RemoveInstance(s.running, k); err != nil {
						log.Errorf("Failed to remove instance: %s", err)
					} else {
						return nil
					}
				}
			}
		}
	} else {
		log.Info("Prune skipping due to Immutable scheduler state.")
	}
	return nil
}

// SerialLauncher performs the launching of all tasks in a time-limited
// way.  This helps to prevent misconfiguration by allowing time for state
// to propagate.
func (s *EtcdScheduler) SerialLauncher(driver scheduler.SchedulerDriver) {
	for {
		// pauseChan needs priority over launchChan, so we need to try it
		// before randomly selecting from either of them below.
		for {
			select {
			case <-s.pauseChan:
				log.V(2).Infof("SerialLauncher sleeping for %d seconds "+
					"after receiving pause signal.", s.chillSeconds)
				time.Sleep(s.chillSeconds * time.Second)
			default:
				goto FCFSPauseOrLaunch
			}
		}
	FCFSPauseOrLaunch:
		select {
		case _, ok := <-s.launchChan:
			if !ok {
				return
			}
			s.launchOne(driver)

			// Wait some time between launches to allow a cluster to settle.
			log.V(2).Infof("SerialLauncher sleeping for %d seconds after "+
				"launch attempt.", s.chillSeconds)
			time.Sleep(s.chillSeconds * time.Second)
		case <-s.pauseChan:
			log.V(2).Infof("SerialLauncher sleeping for %d seconds "+
				"after receiving pause signal.", s.chillSeconds)
			time.Sleep(s.chillSeconds * time.Second)
		}
	}
}

func (s *EtcdScheduler) shouldLaunch(driver scheduler.SchedulerDriver) bool {
	s.mut.RLock()
	defer s.mut.RUnlock()

	if s.state != Mutable {
		log.Infoln("Scheduler is not mutable.  Not launching a task.")
		return false
	}

	if atomic.LoadInt32(&s.reseeding) == reseedUnderway {
		log.Infoln("Currently reseeding, not launching a task.")
		return false
	}

	if len(s.pending) != 0 {
		log.Infoln("Waiting on pending task to fail or submit status. " +
			"Not launching until we hear back.")
		return false
	}

	log.V(2).Infof("running: %+v", s.running)
	if len(s.running) >= s.desiredInstanceCount {
		log.V(2).Infoln("Already running enough tasks.")
		return false
	}

	members, err := rpc.MemberList(s.running)
	if err != nil {
		log.Errorf("Failed to retrieve running member list, "+
			"rescheduling launch attempt for later: %s", err)
		return false
	}
	if len(members) == s.desiredInstanceCount {
		log.Errorf("Cluster is already configured for desired number of nodes.  " +
			"Must deconfigure any dead nodes first or we may risk livelock.")
		return false
	}

	err = s.healthCheck(s.running)
	if err != nil {
		atomic.StoreUint32(&s.Stats.IsHealthy, 0)
		atomic.AddUint32(&s.Stats.ClusterLivelocks, 1)
		// If we have been unhealthy for reseedTimeout seconds, it's time to reseed.
		if s.livelockWindow != nil {
			if time.Since(*s.livelockWindow) > s.reseedTimeout {
				log.Errorf("Cluster has been livelocked for longer than %d seconds!",
					s.reseedTimeout/time.Second)
				if s.autoReseedEnabled {
					log.Warningf("Initiating reseed...")
					// Set scheduler to immutable so that shouldLaunch bails out almost
					// instantly, preventing multiple reseed events from occurring concurrently
					go s.reseedCluster(driver)
				} else {
					log.Warning("Automatic reseed disabled (--auto-reseed=false). " +
						"Doing nothing.")
				}
				return false
			}
		} else {
			now := time.Now()
			s.livelockWindow = &now
		}

		log.Errorf("Failed health check, rescheduling "+
			"launch attempt for later: %s", err)
		return false
	}
	atomic.StoreUint32(&s.Stats.IsHealthy, 1)

	// reset livelock window because we're healthy
	s.livelockWindow = nil
	return true
}

// TODO(tyler) split this long function up!
func (s *EtcdScheduler) launchOne(driver scheduler.SchedulerDriver) {
	// Always ensure we've pruned any dead / unmanaged nodes before
	// launching new ones, or we may overconfigure the ensemble such
	// that it can not make progress if the next launch fails.
	err := s.Prune()
	if err != nil {
		log.Errorf("Failed to remove stale cluster members: %s", err)
		return
	}

	if !s.shouldLaunch(driver) {
		log.Infoln("Skipping launch attempt for now.")
		return
	}

	// validOffer filters out offers that are no longer
	// desirable, even though they may have been when
	// they were enqueued.
	validOffer := func(offer *mesos.Offer) bool {
		runningCopy := s.RunningCopy()
		for _, etcdConfig := range runningCopy {
			if etcdConfig.SlaveID == offer.SlaveId.GetValue() {
				if s.singleInstancePerSlave {
					log.Info("Skipping offer: already running on this slave.")
					return false
				}
			}
		}
		return true
	}

	// Issue BlockingPop until we get back an offer we can use.
	var offer *mesos.Offer
	for {
		offer = s.offerCache.BlockingPop()
		if validOffer(offer) {
			break
		} else {
			s.decline(driver, offer)
		}
	}

	// Do this again because BlockingPop may have taken a long time.
	if !s.shouldLaunch(driver) {
		log.Infoln("Skipping launch attempt for now.")
		s.decline(driver, offer)
		return
	}

	// TODO(tyler) this is a broken hack
	resources := parseOffer(offer)
	lowest := *resources.ports[0].Begin
	rpcPort := lowest
	clientPort := lowest + 1
	httpPort := lowest + 2

	s.mut.Lock()
	var clusterType string
	if len(s.running) == 0 {
		clusterType = "new"
	} else {
		clusterType = "existing"
	}

	s.highestInstanceID++
	name := "etcd-" + strconv.FormatInt(s.highestInstanceID, 10)

	node := &config.Node{
		Name:       name,
		Host:       *offer.Hostname,
		RPCPort:    rpcPort,
		ClientPort: clientPort,
		ReseedPort: httpPort,
		Type:       clusterType,
		SlaveID:    offer.GetSlaveId().GetValue(),
	}
	running := []*config.Node{node}
	for _, r := range s.running {
		running = append(running, r)
	}
	serializedNodes, err := json.Marshal(running)
	log.Infof("Serialized running: %+v", string(serializedNodes))
	if err != nil {
		log.Errorf("Could not serialize running list: %v", err)
		// This Unlock is not deferred because the test implementation of LaunchTasks
		// calls this scheduler's StatusUpdate method, causing the test to deadlock.
		s.decline(driver, offer)
		s.mut.Unlock()
		return
	}

	configSummary := node.String()

	taskID := &mesos.TaskID{Value: &configSummary}

	executor := s.newExecutorInfo(node, s.executorUris)
	task := &mesos.TaskInfo{
		Data:     serializedNodes,
		Name:     proto.String("etcd-server"),
		TaskId:   taskID,
		SlaveId:  offer.SlaveId,
		Executor: executor,
		Resources: []*mesos.Resource{
			util.NewScalarResource("cpus", s.cpusPerTask),
			util.NewScalarResource("mem", s.memPerTask),
			util.NewScalarResource("disk", s.diskPerTask),
			util.NewRangesResource("ports", []*mesos.Value_Range{
				util.NewValueRange(uint64(rpcPort), uint64(httpPort)),
			}),
		},
	}

	log.Infof(
		"Prepared task: %s with offer %s for launch",
		task.GetName(),
		offer.Id.GetValue(),
	)
	log.Info("Launching etcd node.")

	tasks := []*mesos.TaskInfo{task}

	s.pending[node.Name] = struct{}{}

	// This Unlock is not deferred because the test implementation of LaunchTasks
	// calls this scheduler's StatusUpdate method, causing the test to deadlock.
	s.mut.Unlock()

	atomic.AddUint32(&s.Stats.LaunchedServers, 1)
	driver.LaunchTasks(
		[]*mesos.OfferID{offer.Id},
		tasks,
		&mesos.Filters{
			RefuseSeconds: proto.Float64(1),
		},
	)
}

func (s *EtcdScheduler) AdminHTTP(port int, driver scheduler.SchedulerDriver) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		log.Infof("Admin HTTP received %s %s", r.Method, r.URL.Path)
		serializedStats, err := json.Marshal(s.Stats)
		if err != nil {
			log.Errorf("Failed to marshal stats json: %v", err)
		}
		fmt.Fprint(w, string(serializedStats))
	})
	mux.HandleFunc("/reseed", func(w http.ResponseWriter, r *http.Request) {
		log.Infof("Admin HTTP received %s %s", r.Method, r.URL.Path)
		go s.reseedCluster(driver)
		fmt.Fprint(w, string("reseeding"))
	})
	mux.HandleFunc("/members", func(w http.ResponseWriter, r *http.Request) {
		log.Infof("Admin HTTP received %s %s", r.Method, r.URL.Path)
		running := []*config.Node{}
		for _, r := range s.RunningCopy() {
			running = append(running, r)
		}
		serializedNodes, err := json.Marshal(running)
		if err != nil {
			log.Errorf("Failed to marshal members json: %v", err)
		}
		fmt.Fprint(w, string(serializedNodes))
	})
	log.Infof("Admin HTTP interface Listening on port %d", port)
	log.Error(http.ListenAndServe(fmt.Sprintf(":%d", port), mux))
	if s.shutdown != nil {
		s.shutdown()
	}
}

func (s *EtcdScheduler) reseedCluster(driver scheduler.SchedulerDriver) {
	// This CAS allows us to:
	//	1. ensure non-concurrent execution
	//	2. signal to shouldLaunch that we're already reseeding
	if !atomic.CompareAndSwapInt32(&s.reseeding, notReseeding, reseedUnderway) {
		return
	}
	atomic.AddUint32(&s.Stats.ClusterReseeds, 1)
	candidates := rpc.RankReseedCandidates(s.running)
	if len(candidates) == 0 {
		log.Error("Failed to retrieve any candidates for reseeding! " +
			"No recovery possible!")
		driver.Abort()
	}

	s.mut.Lock()
	defer s.mut.Unlock()
	s.state = Immutable

	killable := []string{}
	newSeed := ""
	log.Infof("Candidates for reseed: %+v", candidates)
	for _, node := range candidates {
		// 1. restart node with --force-new-cluster
		// 2. ensure it passes health check
		// 3. ensure its member list only contains itself
		// 4. kill everybody else
		if newSeed != "" {
			log.Warningf("Marking node %s from previous cluster as inferior", node.Node)
			killable = append(killable, node.Node)
		} else {
			log.Warningf("Attempting to re-seed cluster with candidate %s "+
				"with Raft index %d!", node.Node, node.RaftIndex)
			if s.reseedNode(node.Node, driver) {
				newSeed = node.Node
				continue
			}
			// Mark this node as killable, as it did not become healthy on time.
			log.Errorf("Failed reseed attempt on node %s, trying the next-best node.",
				node.Node)
			log.Warningf("Marking node %s from previous cluster as inferior", node.Node)
			killable = append(killable, node.Node)
		}
	}
	if newSeed != "" {
		log.Warningf("We think we have a new healthy leader.")
		for _, node := range killable {
			driver.KillTask(s.tasks[node])
		}
	}
	atomic.StoreInt32(&s.reseeding, notReseeding)
	s.state = Mutable
}

func (s *EtcdScheduler) reseedNode(node string, driver scheduler.SchedulerDriver) bool {
	// Try to reseed with this node
	rpc.TriggerReseed(s.running[node])
	// Wait for it to become healthy, but if it doesn't then kill it
	backoff := 1
	before := time.Now()
	for time.Since(before) < s.reseedTimeout {
		err := rpc.HealthCheck(map[string]*config.Node{
			node: s.running[node],
		})
		if err == nil {
			log.Warningf("Picked node %s to be the new seed!", node)
			return true
		}
		log.Warningf("Reseed candidate %s not yet healthy.", node)
		time.Sleep(time.Duration(backoff) * time.Second)
		backoff = int(math.Min(float64(backoff<<1), 8))
	}
	return false
}

func parseOffer(offer *mesos.Offer) OfferResources {
	getResources := func(resourceName string) []*mesos.Resource {
		return util.FilterResources(
			offer.Resources,
			func(res *mesos.Resource) bool {
				return res.GetName() == resourceName
			},
		)
	}

	cpuResources := getResources("cpus")
	cpus := 0.0
	for _, res := range cpuResources {
		cpus += res.GetScalar().GetValue()
	}

	memResources := getResources("mem")
	mems := 0.0
	for _, res := range memResources {
		mems += res.GetScalar().GetValue()
	}

	portResources := getResources("ports")
	ports := make([]*mesos.Value_Range, 0, 10)
	for _, res := range portResources {
		ranges := res.GetRanges()
		ports = append(ports, ranges.GetRange()...)
	}

	diskResources := getResources("disk")
	disk := 0.0
	for _, res := range diskResources {
		disk += res.GetScalar().GetValue()
	}

	return OfferResources{
		cpus:  cpus,
		mems:  mems,
		disk:  disk,
		ports: ports,
	}
}

func ServeExecutorArtifact(path, address string, artifactPort int) (*string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	serveFile := func(pattern string, filename string) {
		http.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, filename)
		})
	}

	// Create base path (http://foobar:5000/<base>)
	pathSplit := strings.Split(path, "/")
	var base string
	if len(pathSplit) > 0 {
		base = pathSplit[len(pathSplit)-1]
	} else {
		base = path
	}
	serveFile("/"+base, path)

	hostURI := fmt.Sprintf("http://%s:%d/%s", address, artifactPort, base)
	log.V(2).Infof("Hosting artifact '%s' at '%s'", path, hostURI)

	return &hostURI, nil
}

func (s *EtcdScheduler) newExecutorInfo(
	node *config.Node,
	executorURIs []*mesos.CommandInfo_URI,
) *mesos.ExecutorInfo {

	_, bin := filepath.Split(s.ExecutorPath)
	execmd := fmt.Sprintf("./%s -log_dir=./", bin)

	return &mesos.ExecutorInfo{
		ExecutorId: util.NewExecutorID(node.Name),
		Name:       proto.String("etcd"),
		Source:     proto.String("go_test"),
		Command: &mesos.CommandInfo{
			Value: proto.String(execmd),
			Uris:  executorURIs,
		},
		Resources: []*mesos.Resource{
			util.NewScalarResource("cpus", 0.1),
			util.NewScalarResource("mem", 32),
		},
	}
}
