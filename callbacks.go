package main

import (
	"time"

	"github.com/Nitro/sidecar-executor/container"
	"github.com/mesos/mesos-go/api/v0/executor"
	mesos "github.com/mesos/mesos-go/api/v0/mesosproto"
	"github.com/relistan/go-director"
	log "github.com/sirupsen/logrus"
)

// Callbacks from the Mesos driver. These are required to implement
// the Executor interface.

func (exec *sidecarExecutor) Registered(driver executor.ExecutorDriver,
	execInfo *mesos.ExecutorInfo, fwinfo *mesos.FrameworkInfo, slaveInfo *mesos.SlaveInfo) {
	log.Info("Registered Executor on slave ", slaveInfo.GetHostname())
}

func (exec *sidecarExecutor) Reregistered(driver executor.ExecutorDriver, slaveInfo *mesos.SlaveInfo) {
	log.Info("Re-registered Executor on slave ", slaveInfo.GetHostname())
}

func (exec *sidecarExecutor) Disconnected(driver executor.ExecutorDriver) {
	log.Info("Executor disconnected.")
}

// LaunchTask is a callback from Mesos driver to launch a new task in this
// executor.
func (exec *sidecarExecutor) LaunchTask(driver executor.ExecutorDriver, taskInfo *mesos.TaskInfo) {
	log.Infof("Launching task %s with command '%s'", taskInfo.GetName(), taskInfo.Command.GetValue())
	log.Info("Task ID ", taskInfo.GetTaskId().GetValue())

	// We need to tell the scheduler that we started the task
	exec.sendStatus(TaskRunning, taskInfo.GetTaskId())

	log.Infof("Using image '%s'", *taskInfo.Container.Docker.Image)

	// TODO implement configurable pull timeout?
	if !container.CheckImage(exec.client, taskInfo) || *taskInfo.Container.Docker.ForcePullImage {
		err := container.PullImage(exec.client, taskInfo, exec.dockerAuth)
		if err != nil {
			log.Errorf("Failed to pull image: %s", err)
			exec.failTask(taskInfo)
			return
		}
	} else {
		log.Info("Re-using existing image... already present")
	}

	// Additional environment variables we'll pass to the container
	var addEnvVars []string
	if exec.config.SeedSidecar {
		// Fetch the Mesos slave list and append the SIDECAR_SEEDS env var
		addEnvVars = exec.addSidecarSeeds(addEnvVars)
	}

	// Configure the container
	containerConfig := container.ConfigForTask(
		taskInfo,
		exec.config.ForceCpuLimit,
		exec.config.ForceMemoryLimit,
		addEnvVars,
	)

	dockerLabels := container.LabelsForTask(taskInfo)

	// Log out what we're starting up with
	exec.logTaskEnv(taskInfo, dockerLabels, addEnvVars)

	// Try to decrypt any existing Vault encoded env.
	decryptedEnv, err := exec.vault.DecryptAllEnv(containerConfig.Config.Env)
	if err != nil {
		log.Error(err.Error())
		exec.failTask(taskInfo)
		return
	}
	containerConfig.Config.Env = decryptedEnv

	// create the container
	cntnr, err := exec.client.CreateContainer(*containerConfig)
	if err != nil {
		log.Errorf("Failed to create Docker container: %s", err)
		exec.failTask(taskInfo)
		return
	}

	// Start the container
	log.Info("Starting container with ID " + cntnr.ID[:12])
	err = exec.client.StartContainer(cntnr.ID, nil)
	if err != nil {
		log.Errorf("Failed to start Docker container: %s", err)
		exec.failTask(taskInfo)
		return
	}

	// For debugging, set process title to contain container ID & image
	SetProcessName("sidecar-executor " + cntnr.ID[:12] + " (" + *taskInfo.Container.Docker.Image + ")")

	exec.watchLooper =
		director.NewImmediateTimedLooper(director.FOREVER, config.SidecarPollInterval, make(chan error))

	// We have to do this in a different goroutine or the scheduler
	// can't send us any further updates.
	go exec.watchContainer(cntnr.ID, shouldCheckSidecar(containerConfig))
	go exec.monitorTask(cntnr.ID[:12], taskInfo)

	// We may be responsible for log relaying. Handle, if we are.
	exec.handleContainerLogs(cntnr.ID, dockerLabels)

	log.Info("Launched Sidecar tasks... ready for Mesos instructions")
}

// KillTask is a Mesos callback that will try very hard to kill off a running
// task/container.
func (exec *sidecarExecutor) KillTask(driver executor.ExecutorDriver, taskID *mesos.TaskID) {
	log.Infof("Killing task: %s", *taskID.Value)

	// Stop watching the container so we don't send the wrong task status
	go func() { exec.watchLooper.Quit() }()

	containerName := container.GetContainerName(taskID)

	err := container.StopContainer(exec.client, containerName, config.KillTaskTimeout)
	if err != nil {
		log.Errorf("Error stopping container %s! %s", containerName, err.Error())
	}

	// Have to force this to be an int64
	var status int64 = TaskKilled // Default status is that we shot it in the head

	// Now we need to sort out whether the task finished nicely or not.
	// This driver callback is used both to shoot a task in the head, and when
	// a task is being replaced. The Mesos task status needs to reflect the
	// resulting container State.ExitCode.
	container, err := exec.client.InspectContainer(containerName)
	if err == nil {
		if container.State.ExitCode == 0 {
			status = TaskFinished // We exited cleanly when asked
		}
	} else {
		log.Errorf("Error inspecting container %s! %s", containerName, err.Error())
	}

	// Copy the failure logs (hopefully) to stdout/stderr so we can get them
	if !exec.config.ContainerLogsStdout {
		exec.copyLogs(containerName)
	}
	// Notify Mesos
	exec.sendStatus(status, taskID)

	// We have to give the driver time to send the message
	time.Sleep(StatusSleepTime)

	log.Info("Executor believes container has exited, stopping Mesos driver")
	exec.driver.Stop()
}

func (exec *sidecarExecutor) FrameworkMessage(driver executor.ExecutorDriver, msg string) {
	log.Info("Got framework message: ", msg)
}

func (exec *sidecarExecutor) Shutdown(driver executor.ExecutorDriver) {
	log.Info("Shutting down the executor")
}

func (exec *sidecarExecutor) Error(driver executor.ExecutorDriver, err string) {
	log.Info("Got error message:", err)
}
