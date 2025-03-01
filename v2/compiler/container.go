package compiler

import (
	"fmt"

	wfapi "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/golang/protobuf/jsonpb"
	"github.com/kubeflow/pipelines/api/v2alpha1/go/pipelinespec"
	k8score "k8s.io/api/core/v1"
	k8sres "k8s.io/apimachinery/pkg/api/resource"
)

func (c *workflowCompiler) Container(name string, component *pipelinespec.ComponentSpec, container *pipelinespec.PipelineDeploymentConfig_PipelineContainerSpec) error {
	if component == nil {
		return fmt.Errorf("workflowCompiler.Container: component spec must be non-nil")
	}
	marshaler := jsonpb.Marshaler{}
	componentJson, err := marshaler.MarshalToString(component)
	if err != nil {
		return fmt.Errorf("workflowCompiler.Container: marlshaling component spec to proto JSON failed: %w", err)
	}
	containerJson, err := marshaler.MarshalToString(container)
	if err != nil {
		return fmt.Errorf("workflowCompiler.Container: marlshaling pipeline container spec to proto JSON failed: %w", err)
	}
	driverTask, driverOutputs := c.containerDriverTask(
		"driver",
		containerDriverInputs{
			component:      inputParameter(paramComponent),
			task:           inputParameter(paramTask),
			container:      containerJson,
			dagExecutionID: inputParameter(paramDAGExecutionID),
			iterationIndex: inputParameter(paramIterationIndex),
		},
	)
	t, err := containerExecutorTemplate(container, c.launcherImage, c.spec.PipelineInfo.GetName())
	if err != nil {
		return err
	}
	// TODO(Bobgy): how can we avoid template name collisions?
	containerTemplateName, err := c.addTemplate(t, name+"-container")
	if err != nil {
		return err
	}
	wrapper := &wfapi.Template{
		Inputs: wfapi.Inputs{
			Parameters: []wfapi.Parameter{
				{Name: paramTask},
				{Name: paramDAGExecutionID},
				// TODO(Bobgy): reuse the entire 2-step container template
				{Name: paramComponent, Default: wfapi.AnyStringPtr(componentJson)},
				{Name: paramIterationIndex, Default: wfapi.AnyStringPtr("-1")},
			},
		},
		DAG: &wfapi.DAGTemplate{
			Tasks: []wfapi.DAGTask{
				*driverTask,
				{
					Name:         "container",
					Template:     containerTemplateName,
					Dependencies: []string{driverTask.Name},
					When:         taskOutputParameter(driverTask.Name, paramCachedDecision) + " != true",
					Arguments: wfapi.Arguments{
						Parameters: []wfapi.Parameter{{
							Name:  paramExecutorInput,
							Value: wfapi.AnyStringPtr(driverOutputs.executorInput),
						}, {
							Name:  paramExecutionID,
							Value: wfapi.AnyStringPtr(driverOutputs.executionID),
						}, {
							Name:  paramComponent,
							Value: wfapi.AnyStringPtr(inputParameter(paramComponent)),
						}},
					}},
			},
		},
	}
	_, err = c.addTemplate(wrapper, name)
	return err
}

type containerDriverOutputs struct {
	executorInput string
	executionID   string
	cached        string
}

type containerDriverInputs struct {
	component      string
	task           string
	container      string
	dagExecutionID string
	iterationIndex string // optional, when this is an iteration task
}

func (c *workflowCompiler) containerDriverTask(name string, inputs containerDriverInputs) (*wfapi.DAGTask, *containerDriverOutputs) {
	dagTask := &wfapi.DAGTask{
		Name:     name,
		Template: c.addContainerDriverTemplate(),
		Arguments: wfapi.Arguments{
			Parameters: []wfapi.Parameter{
				{Name: paramComponent, Value: wfapi.AnyStringPtr(inputs.component)},
				{Name: paramTask, Value: wfapi.AnyStringPtr(inputs.task)},
				{Name: paramContainer, Value: wfapi.AnyStringPtr(inputs.container)},
				{Name: paramDAGExecutionID, Value: wfapi.AnyStringPtr(inputs.dagExecutionID)},
				{Name: paramIterationIndex, Value: wfapi.AnyStringPtr(inputs.iterationIndex)},
			},
		},
	}
	outputs := &containerDriverOutputs{
		executorInput: taskOutputParameter(name, paramExecutorInput),
		executionID:   taskOutputParameter(name, paramExecutionID),
		cached:        taskOutputParameter(name, paramCachedDecision),
	}
	return dagTask, outputs
}

func (c *workflowCompiler) addContainerDriverTemplate() string {
	name := "system-container-driver"
	_, ok := c.templates[name]
	if ok {
		return name
	}
	t := &wfapi.Template{
		Name: name,
		Inputs: wfapi.Inputs{
			Parameters: []wfapi.Parameter{
				{Name: paramComponent},
				{Name: paramTask},
				{Name: paramContainer},
				{Name: paramDAGExecutionID},
				{Name: paramIterationIndex},
			},
		},
		Outputs: wfapi.Outputs{
			Parameters: []wfapi.Parameter{
				{Name: paramExecutionID, ValueFrom: &wfapi.ValueFrom{Path: "/tmp/outputs/execution-id"}},
				{Name: paramExecutorInput, ValueFrom: &wfapi.ValueFrom{Path: "/tmp/outputs/executor-input"}},
				{Name: paramCachedDecision, Default: wfapi.AnyStringPtr("false"), ValueFrom: &wfapi.ValueFrom{Path: "/tmp/outputs/cached-decision", Default: wfapi.AnyStringPtr("false")}},
			},
		},
		Container: &k8score.Container{
			Image:   c.driverImage,
			Command: []string{"driver"},
			Args: []string{
				"--type", "CONTAINER",
				"--pipeline_name", c.spec.GetPipelineInfo().GetName(),
				"--run_id", runID(),
				"--dag_execution_id", inputValue(paramDAGExecutionID),
				"--component", inputValue(paramComponent),
				"--task", inputValue(paramTask),
				"--container", inputValue(paramContainer),
				"--iteration_index", inputValue(paramIterationIndex),
				"--execution_id_path", outputPath(paramExecutionID),
				"--executor_input_path", outputPath(paramExecutorInput),
				"--cached_decision_path", outputPath(paramCachedDecision),
			},
			Resources: driverResources,
		},
	}
	c.templates[name] = t
	c.wf.Spec.Templates = append(c.wf.Spec.Templates, *t)
	return name
}

func containerExecutorTemplate(container *pipelinespec.PipelineDeploymentConfig_PipelineContainerSpec, launcherImage, pipelineName string) (*wfapi.Template, error) {
	userCmdArgs := make([]string, 0, len(container.Command)+len(container.Args))
	userCmdArgs = append(userCmdArgs, container.Command...)
	userCmdArgs = append(userCmdArgs, container.Args...)
	launcherCmd := []string{
		volumePathKFPLauncher + "/launch",
		// TODO(Bobgy): no need to pass pipeline_name and run_id, these info can be fetched via pipeline context and pipeline run context which have been created by root DAG driver.
		"--pipeline_name", pipelineName,
		"--run_id", runID(),
		"--execution_id", inputValue(paramExecutionID),
		"--executor_input", inputValue(paramExecutorInput),
		"--component_spec", inputValue(paramComponent),
		"--pod_name",
		"$(KFP_POD_NAME)",
		"--pod_uid",
		"$(KFP_POD_UID)",
		"--mlmd_server_address", // METADATA_GRPC_SERVICE_* come from metadata-grpc-configmap
		"$(METADATA_GRPC_SERVICE_HOST)",
		"--mlmd_server_port",
		"$(METADATA_GRPC_SERVICE_PORT)",
		"--", // separater before user command and args
	}
	// TODO(Bobgy): support resource limits from parameters: https://github.com/kubeflow/pipelines/issues/6354.
	res := k8score.ResourceRequirements{
		Limits: map[k8score.ResourceName]k8sres.Quantity{},
	}
	memoryLimit := container.GetResources().GetMemoryLimit()
	if memoryLimit != 0 {
		q, err := k8sres.ParseQuantity(fmt.Sprintf("%vG", memoryLimit))
		if err != nil {
			return nil, err
		}
		res.Limits[k8score.ResourceMemory] = q
	}
	cpuLimit := container.GetResources().GetCpuLimit()
	if cpuLimit != 0 {
		q, err := k8sres.ParseQuantity(fmt.Sprintf("%v", cpuLimit))
		if err != nil {
			return nil, err
		}
		res.Limits[k8score.ResourceCPU] = q
	}
	// Normalize to make snapshot testing easier.
	if len(res.Limits) == 0 {
		res.Limits = nil
	}
	if len(res.Requests) == 0 {
		res.Requests = nil
	}
	accelerator := container.GetResources().GetAccelerator()
	if accelerator != nil {
		return nil, fmt.Errorf("accelerator resources are not supported yet: https://github.com/kubeflow/pipelines/issues/7043")
	}
	mlmdConfigOptional := true
	return &wfapi.Template{
		Inputs: wfapi.Inputs{
			Parameters: []wfapi.Parameter{
				{Name: paramExecutorInput},
				{Name: paramExecutionID},
				{Name: paramComponent},
			},
		},
		Volumes: []k8score.Volume{{
			Name: volumeNameKFPLauncher,
			VolumeSource: k8score.VolumeSource{
				EmptyDir: &k8score.EmptyDirVolumeSource{},
			},
		}},
		InitContainers: []wfapi.UserContainer{{
			Container: k8score.Container{
				Name:    "kfp-launcher",
				Image:   launcherImage,
				Command: []string{"launcher-v2", "--copy", "/kfp-launcher/launch"},
				VolumeMounts: []k8score.VolumeMount{{
					Name:      volumeNameKFPLauncher,
					MountPath: volumePathKFPLauncher,
				}},
				ImagePullPolicy: "Always",
				Resources:       launcherResources,
			},
		}},
		Container: &k8score.Container{
			Command: launcherCmd,
			Args:    userCmdArgs,
			Image:   container.Image,
			VolumeMounts: []k8score.VolumeMount{{
				Name:      volumeNameKFPLauncher,
				MountPath: volumePathKFPLauncher,
			}},
			EnvFrom: []k8score.EnvFromSource{{
				ConfigMapRef: &k8score.ConfigMapEnvSource{
					LocalObjectReference: k8score.LocalObjectReference{
						Name: "metadata-grpc-configmap",
					},
					Optional: &mlmdConfigOptional,
				},
			}},
			Env: []k8score.EnvVar{{
				Name: "KFP_POD_NAME",
				ValueFrom: &k8score.EnvVarSource{
					FieldRef: &k8score.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			}, {
				Name: "KFP_POD_UID",
				ValueFrom: &k8score.EnvVarSource{
					FieldRef: &k8score.ObjectFieldSelector{
						FieldPath: "metadata.uid",
					},
				},
			}},
			Resources: res,
		},
	}, nil
}
