package scheduler

import (
	"container/ring"
	"encoding/json"
	"fmt"
	"sync"
	"errors"

	"code.google.com/p/goprotobuf/proto"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	kubernetes "github.com/GoogleCloudPlatform/kubernetes/pkg/scheduler"
	log "github.com/golang/glog"
	"github.com/mesosphere/mesos-go/mesos"
	"gopkg.in/v1/yaml"
	"github.com/mesosphere/kubernetes-mesos/uuid"
)

var errSchedulerTimeout = fmt.Errorf("Schedule time out")

const (
	containerCpus            = 1
	containerMem             = 512
	defaultFinishedTasksSize = 1024
)

// TODO(nnielsen): Rename KubernetesScheduler -> KubernetesScheduler.

// PodScheduleFunc implements how to schedule
// pods among slaves. We can have different implementation
// for different scheduling policy.
//
// The Schedule function takes a group of slaves (each contains offers
// from that slave), and a group of pods.
//
// After deciding which slave to schedule the pod, it fills the task info and the
//'SelectedMachine' channel  with the host name of the slave.
// See the FIFOScheduleFunc for example.
type PodScheduleFunc func(slaves map[string]*Slave, tasks map[string]*PodTask) []*PodTask

// A struct describes a pod task.
type PodTask struct {
	ID              string
	Pod             *api.Pod
	Machines        []string
	SelectedMachine chan string
	TaskInfo        *mesos.TaskInfo
	OfferIds        []string
}

// Fill the TaskInfo in the PodTask, should be called in PodScheduleFunc.
func (t *PodTask) FillTaskInfo(slaveId string, offers ...*mesos.Offer) {
	for _, offer := range offers {
		t.OfferIds = append(t.OfferIds, offer.GetId().GetValue())
	}

	var err error

	t.TaskInfo.TaskId = &mesos.TaskID{Value: proto.String(t.ID)}
	t.TaskInfo.SlaveId = &mesos.SlaveID{Value: proto.String(slaveId)}
	t.TaskInfo.Resources = []*mesos.Resource{
		mesos.ScalarResource("cpus", containerCpus),
		mesos.ScalarResource("mem", containerMem),
	}
	t.TaskInfo.Data, err = yaml.Marshal(&t.Pod.DesiredState.Manifest)
	if err != nil {
		log.Warningf("Failed to marshal the manifest")
	}
}

func newPodTask(pod *api.Pod, executor *mesos.ExecutorInfo, machines []string) (*PodTask, error) {
	taskId, err := uuid.Gen()
	if err != nil {
		return nil, errors.New("Failed to generate task id: " + err.Error())
	}

	task := &PodTask{
		ID:              taskId, // pod.JSONBase.ID,
		Pod:             pod,
		Machines:        make([]string, len(machines)),
		SelectedMachine: make(chan string, 1),
		TaskInfo:        new(mesos.TaskInfo),
	}
	copy(task.Machines, machines)
	task.TaskInfo.Name = proto.String("PodTask")
	task.TaskInfo.Executor = executor
	return task, nil
}

// A struct that describes the slave.
type Slave struct {
	HostName string
	Offers   map[string]*mesos.Offer
	tasks    map[string]*PodTask
}

func newSlave(hostName string) *Slave {
	return &Slave{
		HostName: hostName,
		Offers:   make(map[string]*mesos.Offer),
		tasks:    make(map[string]*PodTask),
	}
}

// KubernetesScheduler implements:
// 1: The interfaces of the mesos scheduler.
// 2: The interface of a kubernetes scheduler.
// 3: The interfaces of a kubernetes pod registry.
type KubernetesScheduler struct {
	// We use a lock here to avoid races
	// between invoking the mesos callback
	// and the invoking the pob registry interfaces.
	*sync.RWMutex

	// Mesos context.
	executor    *mesos.ExecutorInfo
	Driver      mesos.SchedulerDriver
	frameworkId *mesos.FrameworkID
	masterInfo  *mesos.MasterInfo
	registered  bool

	// OfferID => offer.
	offers map[string]*mesos.Offer

	// SlaveID => slave.
	slaves map[string]*Slave

	// Slave's hostname => slaveID
	slaveIDs map[string]string

	// Fields for task information book keeping.
	pendingTasks  map[string]*PodTask
	runningTasks  map[string]*PodTask
	finishedTasks *ring.Ring

	podToTask map[string]string

	// The function that does scheduling.
	scheduleFunc PodScheduleFunc
}

// New create a new KubernetesScheduler
func New(executor *mesos.ExecutorInfo, scheduleFunc PodScheduleFunc) *KubernetesScheduler {
	return &KubernetesScheduler{
		new(sync.RWMutex),
		executor,
		nil,
		nil,
		nil,
		false,
		make(map[string]*mesos.Offer),
		make(map[string]*Slave),
		make(map[string]string),
		make(map[string]*PodTask),
		make(map[string]*PodTask),
		ring.New(defaultFinishedTasksSize),
		make(map[string]string),
		scheduleFunc,
	}
}

// Registered is called when the scheduler registered with the master successfully.
func (k *KubernetesScheduler) Registered(driver mesos.SchedulerDriver,
	frameworkId *mesos.FrameworkID, masterInfo *mesos.MasterInfo) {
	k.frameworkId = frameworkId
	k.masterInfo = masterInfo
	k.registered = true
	log.Infof("Scheduler registered with the master: %v with frameworkId: %v\n", masterInfo, frameworkId)
}

// Reregistered is called when the scheduler re-registered with the master successfully.
// This happends when the master fails over.
func (k *KubernetesScheduler) Reregistered(driver mesos.SchedulerDriver, masterInfo *mesos.MasterInfo) {
	log.Infof("Scheduler reregistered with the master: %v with frameworkId: %v\n", masterInfo)
	k.registered = true
}

// Disconnected is called when the scheduler loses connection to the master.
func (k *KubernetesScheduler) Disconnected(driver mesos.SchedulerDriver) {
	log.Infof("Master disconnected!\n")
	k.registered = false
}

// ResourceOffers is called when the scheduler receives some offers from the master.
func (k *KubernetesScheduler) ResourceOffers(driver mesos.SchedulerDriver, offers []*mesos.Offer) {
	log.Infof("Received offers\n")
	log.V(2).Infof("%v\n", offers)

	k.Lock()
	defer k.Unlock()

	// Record the offers in the global offer map as well as each slave's offer map.
	for _, offer := range offers {
		offerId := offer.GetId().GetValue()
		slaveId := offer.GetSlaveId().GetValue()

		slave, exists := k.slaves[slaveId]
		if !exists {
			k.slaves[slaveId] = newSlave(offer.GetHostname())
			slave = k.slaves[slaveId]
		}
		slave.Offers[offerId] = offer
		k.offers[offerId] = offer
		k.slaveIDs[slave.HostName] = slaveId
	}
	k.doSchedule()
}

// OfferRescinded is called when the resources are recinded from the scheduler.
func (k *KubernetesScheduler) OfferRescinded(driver mesos.SchedulerDriver, offerId *mesos.OfferID) {
	log.Infof("Offer rescinded %v\n", offerId)

	k.Lock()
	defer k.Unlock()

	slaveId := k.offers[offerId.GetValue()].GetSlaveId().GetValue()
	delete(k.offers, offerId.GetValue())
	delete(k.slaves[slaveId].Offers, offerId.GetValue())
}

// StatusUpdate is called when a status update message is sent to the scheduler.
func (k *KubernetesScheduler) StatusUpdate(driver mesos.SchedulerDriver, taskStatus *mesos.TaskStatus) {
	log.Infof("Received status update %v\n", taskStatus)

	k.Lock()
	defer k.Unlock()

	switch taskStatus.GetState() {
	case mesos.TaskState_TASK_STAGING:
		k.handleTaskStaging(taskStatus)
	case mesos.TaskState_TASK_STARTING:
		k.handleTaskStarting(taskStatus)
	case mesos.TaskState_TASK_RUNNING:
		k.handleTaskRunning(taskStatus)
	case mesos.TaskState_TASK_FINISHED:
		k.handleTaskFinished(taskStatus)
	case mesos.TaskState_TASK_FAILED:
		k.handleTaskFailed(taskStatus)
	case mesos.TaskState_TASK_KILLED:
		k.handleTaskKilled(taskStatus)
	case mesos.TaskState_TASK_LOST:
		k.handleTaskLost(taskStatus)
	}
}

func (k *KubernetesScheduler) handleTaskStaging(taskStatus *mesos.TaskStatus) {
	log.Errorf("Not implemented: task staging")
}

func (k *KubernetesScheduler) handleTaskStarting(taskStatus *mesos.TaskStatus) {
	log.Errorf("Not implemented: task starting")
}

func (k *KubernetesScheduler) handleTaskRunning(taskStatus *mesos.TaskStatus) {
	taskId, slaveId := taskStatus.GetTaskId().GetValue(), taskStatus.GetSlaveId().GetValue()
	slave, exists := k.slaves[slaveId]
	if !exists {
		log.Warningf("Ignore status TASK_RUNNING because the slave does not exist\n")
		return
	}
	task, exists := k.pendingTasks[taskId]
	if !exists {
		log.Warningf("Ignore status TASK_RUNNING (%s) because the the task is discarded: '%v'", taskId, k.pendingTasks)
		return
	}
	if _, exists = k.runningTasks[taskId]; exists {
		log.Warningf("Ignore status TASK_RUNNING because the the task is already running")
		return
	}
	if containsTask(k.finishedTasks, taskId) {
		log.Warningf("Ignore status TASK_RUNNING because the the task is already finished")
		return
	}

	log.Infof("Received running status: '%v'", taskStatus)

	task.Pod.CurrentState.Status = api.PodRunning
	task.Pod.CurrentState.Manifest = task.Pod.DesiredState.Manifest

	if taskStatus.Data != nil {
		var target api.PodInfo
		err := json.Unmarshal(taskStatus.Data, &target)
		if err == nil {
			task.Pod.CurrentState.Info = target
		}
	}

	k.runningTasks[taskId] = task
	slave.tasks[taskId] = task
	delete(k.pendingTasks, taskId)
}

func (k *KubernetesScheduler) handleTaskFinished(taskStatus *mesos.TaskStatus) {
	taskId, slaveId := taskStatus.GetTaskId().GetValue(), taskStatus.GetSlaveId().GetValue()
	slave, exists := k.slaves[slaveId]
	if !exists {
		log.Warningf("Ignore status TASK_FINISHED because the slave does not exist\n")
		return
	}
	if _, exists := k.pendingTasks[taskId]; exists {
		panic("Pending task finished, this couldn't happen")
	}
	if _, exists := k.runningTasks[taskId]; exists {
		log.Warningf("Ignore status TASK_FINISHED because the the task is not running")
		return
	}
	if containsTask(k.finishedTasks, taskId) {
		log.Warningf("Ignore status TASK_FINISHED because the the task is already finished")
		return
	}

	k.finishedTasks.Next().Value = taskId
	delete(k.runningTasks, taskId)
	delete(slave.tasks, taskId)
}

func (k *KubernetesScheduler) handleTaskFailed(taskStatus *mesos.TaskStatus) {
	log.Errorf("Task failed: '%v'", taskStatus)

	taskId := taskStatus.GetTaskId().GetValue()

	if _, exists := k.pendingTasks[taskId]; exists {
		delete(k.pendingTasks, taskId)
	}
	if _, exists := k.runningTasks[taskId]; exists {
		delete(k.runningTasks, taskId)
	}
}

func (k *KubernetesScheduler) handleTaskKilled(taskStatus *mesos.TaskStatus) {
	log.Errorf("Task killed: '%v'", taskStatus)
	taskId := taskStatus.GetTaskId().GetValue()

	if _, exists := k.pendingTasks[taskId]; exists {
		delete(k.pendingTasks, taskId)
	}
	if _, exists := k.runningTasks[taskId]; exists {
		delete(k.runningTasks, taskId)
	}
}

func (k *KubernetesScheduler) handleTaskLost(taskStatus *mesos.TaskStatus) {
	log.Errorf("Task lost: '%v'", taskStatus)
	taskId := taskStatus.GetTaskId().GetValue()

	if _, exists := k.pendingTasks[taskId]; exists {
		delete(k.pendingTasks, taskId)
	}
	if _, exists := k.runningTasks[taskId]; exists {
		delete(k.runningTasks, taskId)
	}
}

// FrameworkMessage is called when the scheduler receives a message from the executor.
func (k *KubernetesScheduler) FrameworkMessage(driver mesos.SchedulerDriver,
	executorId *mesos.ExecutorID, slaveId *mesos.SlaveID, message string) {
	log.Infof("Received messages from executor %v of slave %v, %v\n", executorId, slaveId, message)
}

// SlaveLost is called when some slave is lost.
func (k *KubernetesScheduler) SlaveLost(driver mesos.SchedulerDriver, slaveId *mesos.SlaveID) {
	log.Infof("Slave %v is lost\n", slaveId)
	// TODO(yifan): Restart any unfinished tasks on that slave.
}

// ExecutorLost is called when some executor is lost.
func (k *KubernetesScheduler) ExecutorLost(driver mesos.SchedulerDriver,
	executorId *mesos.ExecutorID, slaveId *mesos.SlaveID, status int) {
	log.Infof("Executor %v of slave %v is lost, status: %v\n", executorId, slaveId, status)
	// TODO(yifan): Restart any unfinished tasks of the executor.
}

// Error is called when there is some error.
func (k *KubernetesScheduler) Error(driver mesos.SchedulerDriver, message string) {
	log.Errorf("Scheduler error: %v\n", message)
}

// Schedule implements the Scheduler interface of the Kubernetes.
// It returns the selectedMachine's name and error (if there's any).
func (k *KubernetesScheduler) Schedule(pod api.Pod, minionLister kubernetes.MinionLister) (string, error) {
	log.Infof("Try to schedule pod\n")
	machineLists, err := minionLister.List()
	if err != nil {
		log.Warningf("minionLister.List() error %v\n", err)
		return "", err
	}

	task, err := newPodTask(&pod, k.executor, machineLists)
	if err != nil {
		return "", err
	}

	k.Lock()
	k.podToTask[pod.JSONBase.ID] = task.ID
	k.pendingTasks[task.ID] = task
	k.doSchedule()
	k.Unlock()

	return <-task.SelectedMachine, nil
}

// Call ScheduleFunc and substract some resources.
func (k *KubernetesScheduler) doSchedule() {
	if tasks := k.scheduleFunc(k.slaves, k.pendingTasks); tasks != nil {
		// Substract offers.
		for _, task := range tasks {
			for _, offerId := range task.OfferIds {
				slaveId := task.TaskInfo.GetSlaveId().GetValue()
				delete(k.offers, offerId)
				delete(k.slaves[slaveId].Offers, offerId)
			}
		}
	}
}

// ListPods obtains a list of pods that match selector.
func (k *KubernetesScheduler) ListPods(selector labels.Selector) ([]api.Pod, error) {
	log.V(2).Infof("List pods for '%v'\n", selector)

	k.RLock()
	defer k.RUnlock()

	var result []api.Pod
	for _, task := range k.runningTasks {
		pod := *(task.Pod)

		var l labels.Set = pod.Labels
		if selector.Matches(l) {
			result = append(result, *(task.Pod))
		}
	}

	// TODO(nnielsen): Refactor tasks append for the three lists.
	for _, task := range k.pendingTasks {
		pod := *(task.Pod)

		var l labels.Set = pod.Labels
		if selector.Matches(l) {
			result = append(result, *(task.Pod))
		}
	}

	// TODO(nnielsen): Wire up check in finished tasks

	log.V(2).Infof("Returning pods: '%v'\n", result)

	return result, nil
}

// Get a specific pod.
func (k *KubernetesScheduler) GetPod(podID string) (*api.Pod, error) {
	log.V(2).Infof("Get pod '%s'\n", podID)
	k.RLock()
	defer k.RUnlock()

	taskId, exists := k.podToTask[podID]
	if !exists {
		return nil, fmt.Errorf("Could not resolve pod '%s' to task id", podID)
	}

	if task, exists := k.pendingTasks[taskId]; exists {
		// return nil, fmt.Errorf("Pod '%s' is still pending", podID)
		log.V(2).Infof("Pending Pod '%s': %v", podID, task.Pod)
		return task.Pod, nil
	}
	if containsTask(k.finishedTasks, taskId) {
		return nil, fmt.Errorf("Pod '%s' is finished", podID)
	}
	if task, exists := k.runningTasks[taskId]; exists {
		log.V(2).Infof("Running Pod '%s': %v", podID, task.Pod)
		return task.Pod, nil
	}
	return nil, fmt.Errorf("Unknown Pod %v", podID)
}

// Create a pod based on a specification, schedule it onto a specific machine.
func (k *KubernetesScheduler) CreatePod(machine string, pod api.Pod) error {
	log.V(2).Infof("Create pod: '%v'\n", pod)
	podID := pod.JSONBase.ID
	taskId, exists := k.podToTask[podID]
	if !exists {
		return fmt.Errorf("Could not resolve pod '%s' to task id", podID)
	}

	task, exists := k.pendingTasks[taskId]
	if !exists {
		return fmt.Errorf("Pod Task does not exist %v\n", taskId)
	}

	// TODO(yifan): By this time, there is a chance that the slave is disconnected.
	offerId := &mesos.OfferID{Value: proto.String(task.OfferIds[0])}
	k.Driver.LaunchTasks(offerId, []*mesos.TaskInfo{task.TaskInfo}, nil)
	return nil
}

// Update an existing pod.
func (k *KubernetesScheduler) UpdatePod(pod api.Pod) error {
	// TODO(yifan): Need to send a special message to the slave/executor.
	return fmt.Errorf("Not implemented: UpdatePod")
}

// Delete an existing pod.
func (k *KubernetesScheduler) DeletePod(podID string) error {
	log.V(2).Infof("Delete pod '%s'\n", podID)
	taskId, exists := k.podToTask[podID]
	if !exists {
		return fmt.Errorf("Could not resolve pod '%s' to task id", podID)
	}

	if task, exists := k.runningTasks[taskId]; exists {
		taskId := &mesos.TaskID{Value: proto.String(task.ID)}
		return k.Driver.KillTask(taskId)
	}

	return fmt.Errorf("Cannot kill pod '%s': pod not found", podID)
}

// A FCFS scheduler.
func FCFSScheduleFunc(slaves map[string]*Slave, tasks map[string]*PodTask) []*PodTask {
	for _, task := range tasks {
		for slaveId, slave := range slaves {
			if !containsHostName(task.Machines, slave.HostName) {
				continue
			}
			for _, offer := range slave.Offers {
				// Fill the task info.
				task.FillTaskInfo(slaveId, offer)
				task.SelectedMachine <- slave.HostName
				// Just schedule one task for now.
				return []*PodTask{task}
			}
		}
	}
	return nil
}

func containsHostName(machines []string, machine string) bool {
	for _, m := range machines {
		if m == machine {
			return true
		}
	}
	return false
}

func containsTask(finishedTasks *ring.Ring, taskId string) bool {
	for i := 0; i < defaultFinishedTasksSize; i++ {
		value := finishedTasks.Next().Value
		if value == nil {
			continue
		}
		if value.(string) == taskId {
			return true
		}
	}
	return false
}
