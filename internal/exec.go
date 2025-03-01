package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/ecs/ecsiface"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/spf13/viper"
)

// ExecCommand is a struct that contains information about our command state
type ExecCommand struct {
	input      chan string
	err        chan error
	done       chan bool
	client     ecsiface.ECSAPI
	region     string
	endpoint   string
	cluster    string
	service    string
	task       *ecs.Task
	tasks      map[string]*ecs.Task
	container  *ecs.Container
	containers []*ecs.Container
}

// CreateExecCommand initialises a new ExecCommand struct with the required initial values
func CreateExecCommand() *ExecCommand {
	client := createEcsClient()
	e := &ExecCommand{
		input:    make(chan string, 1),
		err:      make(chan error, 1),
		done:     make(chan bool),
		client:   client,
		region:   client.SigningRegion,
		endpoint: client.Endpoint,
	}

	return e
}

// Start begins a goroutine that listens on the input channel for instructions
func (e *ExecCommand) Start() {
	// Before we do anything make sure that the session-manager-plugin is available in $PATH, exit if it isn't
	_, err := exec.LookPath("session-manager-plugin")
	if err != nil {
		fmt.Println(red("session-manager-plugin isn't installed or wasn't found in $PATH - https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html"))
		os.Exit(1)
	}

	go func() {
		for {
			select {
			case input := <-e.input:
				switch input {
				case "getCluster":
					e.getCluster()
				case "getService":
					e.getService()
				case "getTask":
					e.getTask()
				case "getContainer":
					e.getContainer()
				case "executeCommand":
					e.executeInput()
				default:
					e.getCluster()
				}
			case err := <-e.err:
				fmt.Printf(red("\n%s\n"), err)
				os.Exit(1)
			}
		}
	}()

	// Initiate the workflow
	e.input <- "getCluster"
	// Block until we receive a message on the done channel
	<-e.done
}

// Lists available clusters and prompts the user to select one
func (e *ExecCommand) getCluster() {
	list, err := e.client.ListClusters(&ecs.ListClustersInput{})
	if err != nil {
		e.err <- err
		return
	}

	var clusterNames []string
	for _, c := range list.ClusterArns {
		arnSplit := strings.Split(*c, "/")
		name := arnSplit[len(arnSplit)-1]
		clusterNames = append(clusterNames, name)
	}

	switch {
	case len(clusterNames) == 1:
		e.cluster = clusterNames[0]
		e.input <- "getService"
		return
	case len(clusterNames) > 1:
		selection, err := selectCluster(clusterNames)
		if err != nil {
			e.err <- err
			return
		}

		e.cluster = selection
		e.input <- "getService"
		return
	default:
		err := errors.New("No clusters found in account or region")
		e.err <- err
		return
	}
}

// Lists available service and prompts the user to select one
func (e *ExecCommand) getService() {
	list, err := e.client.ListServices(&ecs.ListServicesInput{
		Cluster: aws.String(e.cluster),
	})
	if err != nil {
		e.err <- err
		return
	}

	var serviceNames []string
	for _, c := range list.ServiceArns {
		arnSplit := strings.Split(*c, "/")
		name := arnSplit[len(arnSplit)-1]
		serviceNames = append(serviceNames, name)
	}

	if len(serviceNames) > 0 {
		selection, err := selectService(serviceNames)
		if err != nil {
			e.err <- err
			return
		}

		if selection == backOpt {
			e.input <- "getCluster"
			return
		}

		e.service = selection
		e.input <- "getTask"
		return

	} else {
		// Continue without setting a service if no services are found in the cluster
		fmt.Printf(yellow("\n%s"), "No services found in the cluster, returning all running tasks...")
		e.input <- "getTask"
		return
	}
}

// Lists tasks in a cluster and prompts the user to select one
func (e *ExecCommand) getTask() {
	var input *ecs.ListTasksInput

	// If no service has been set, or if ALL (*) services have been selected
	// then we don't need to specify a ServiceName
	if e.service == "" || e.service == "*" {
		input = &ecs.ListTasksInput{
			Cluster: aws.String(e.cluster),
		}
	} else {
		input = &ecs.ListTasksInput{
			Cluster:     aws.String(e.cluster),
			ServiceName: aws.String(e.service),
		}
	}

	list, err := e.client.ListTasks(input)
	if err != nil {
		e.err <- err
		return
	}

	e.tasks = make(map[string]*ecs.Task)
	if len(list.TaskArns) > 0 {
		describe, err := e.client.DescribeTasks(&ecs.DescribeTasksInput{
			Cluster: aws.String(e.cluster),
			Tasks:   list.TaskArns,
		})
		if err != nil {
			e.err <- err
			return
		}

		for _, t := range describe.Tasks {
			taskId := strings.Split(*t.TaskArn, "/")[2]
			e.tasks[taskId] = t
		}

		if len(e.tasks) == 1 {
			for _, t := range e.tasks {
				e.task = t
				e.input <- "getContainer"
				return
			}
		}

		selection, err := selectTask(e.tasks)
		if err != nil {
			e.err <- err
			return
		}

		if *selection.TaskArn == backOpt {
			if e.service == "" {
				e.input <- "getCluster"
				return
			}
			e.input <- "getService"
			return
		}

		e.task = selection
		e.input <- "getContainer"
		return

	} else {
		err := errors.New(fmt.Sprintf("There are no running tasks in the cluster %s", e.cluster))
		e.err <- err
		return
	}
}

// Lists containers in a task and prompts the user to select one (if there is more than 1 container)
// otherwise returns the the only container in the task
func (e *ExecCommand) getContainer() {
	switch {
	case len(e.task.Containers) == 1:
		e.container = e.task.Containers[0]
		e.input <- "executeCommand"
		return
	case len(e.task.Containers) > 1:
		selection, err := selectContainer(e.task.Containers)
		if err != nil {
			e.err <- err
			return
		}

		if *selection.Name == backOpt {
			e.input <- "getTask"
			return
		}

		e.container = selection
		e.input <- "executeCommand"
		return

	default:
		// There is only one container in the task, return it
		e.container = e.task.Containers[0]
		e.input <- "executeCommand"
		return
	}
}

// executeInput takes all of our previous values and builds a session for us
// and then calls runCommand to execute the session input via session-manager-plugin
func (e *ExecCommand) executeInput() {
	// Check if command has been passed to the tool, otherwise default to /bin/sh
	var command string
	if viper.GetString("cmd") != "" {
		command = viper.GetString("cmd")
	} else {
		if strings.Contains(*e.task.PlatformFamily, "Windows") {
			command = "powershell.exe"
		} else {
			command = "/bin/sh"
		}
	}

	execCommand, err := e.client.ExecuteCommand(&ecs.ExecuteCommandInput{
		Cluster:     aws.String(e.cluster),
		Interactive: aws.Bool(true),
		Task:        e.task.TaskArn,
		Command:     aws.String(command),
		Container:   e.container.Name,
	})
	if err != nil {
		e.err <- err
		return
	}

	execSess, err := json.MarshalIndent(execCommand.Session, "", "    ")
	if err != nil {
		e.err <- err
		return
	}

	taskArnSplit := strings.Split(*e.task.TaskArn, "/")
	taskID := taskArnSplit[len(taskArnSplit)-1]
	target := ssm.StartSessionInput{
		Target: aws.String(fmt.Sprintf("ecs:%s_%s_%s", e.cluster, taskID, *e.container.RuntimeId)),
	}
	targetJson, err := json.MarshalIndent(target, "", "    ")
	if err != nil {
		e.err <- err
		return
	}

	// Print Cluster/Service/Task information to the console
	fmt.Printf("\nCluster: %v | Service: %v | Task: %s", cyan(e.cluster), magenta(e.service), green(strings.Split(*e.task.TaskArn, "/")[2]))
	fmt.Printf("\nConnecting to container %v", yellow(*e.container.Name))

	// Execute the session-manager-plugin with our task details
	if err = runCommand("session-manager-plugin", string(execSess), e.region, "StartSession", "", string(targetJson), e.endpoint); err != nil {
		e.done <- true
		e.err <- err
		return
	}

	e.done <- true
	return
}
