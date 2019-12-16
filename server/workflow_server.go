package server

import (
	"context"
	"errors"

	"github.com/onepanelio/core/api"
	"github.com/onepanelio/core/manager"
	"github.com/onepanelio/core/model"
	"github.com/onepanelio/core/util"
	"github.com/onepanelio/core/util/ptr"
)

var userError *util.UserError

type WorkflowServer struct {
	resourceManager *manager.ResourceManager
}

func NewWorkflowServer(resourceManager *manager.ResourceManager) *WorkflowServer {
	return &WorkflowServer{resourceManager: resourceManager}
}

func apiWorkflow(wf *model.Workflow) (workflow *api.Workflow) {
	workflow = &api.Workflow{
		Name:   wf.Name,
		Uid:    wf.UID,
		Status: wf.Status,
	}

	if wf.WorkflowTemplate != nil {
		workflow.WorkflowTemplate = &api.WorkflowTemplate{
			Uid:      wf.WorkflowTemplate.UID,
			Name:     wf.WorkflowTemplate.Name,
			Version:  wf.WorkflowTemplate.Version,
			Manifest: wf.WorkflowTemplate.Manifest,
		}
	}

	return
}

func apiWorkflowTemplate(wft *model.WorkflowTemplate) *api.WorkflowTemplate {
	return &api.WorkflowTemplate{
		Uid:      wft.UID,
		Name:     wft.Name,
		Version:  wft.Version,
		Manifest: wft.Manifest,
	}
}

func (s *WorkflowServer) CreateWorkflow(ctx context.Context, req *api.CreateWorkflowRequest) (*api.Workflow, error) {
	workflow := &model.Workflow{
		WorkflowTemplate: &model.WorkflowTemplate{
			UID:     req.Workflow.WorkflowTemplate.Uid,
			Version: req.Workflow.WorkflowTemplate.Version,
		},
	}
	for _, param := range req.Workflow.Parameters {
		workflow.Parameters = append(workflow.Parameters, model.Parameter{
			Name:  param.Name,
			Value: ptr.String(param.Value),
		})
	}

	wf, err := s.resourceManager.CreateWorkflow(req.Namespace, workflow)
	if err != nil {
		return nil, err
	}

	return apiWorkflow(wf), nil
}

func (s *WorkflowServer) GetWorkflow(ctx context.Context, req *api.GetWorkflowRequest) (*api.Workflow, error) {
	wf, err := s.resourceManager.GetWorkflow(req.Namespace, req.Name)
	if errors.As(err, &userError) {
		return nil, userError.GRPCError()
	}

	return apiWorkflow(wf), nil
}

func (s *WorkflowServer) ListWorkflows(ctx context.Context, req *api.ListWorkflowsRequest) (*api.ListWorkflowsResponse, error) {
	return nil, nil
}

func (s *WorkflowServer) CreateWorkflowTemplate(ctx context.Context, req *api.CreateWorkflowTemplateRequest) (*api.WorkflowTemplate, error) {
	workflowTemplate := &model.WorkflowTemplate{
		Name:     req.WorkflowTemplate.Name,
		Manifest: req.WorkflowTemplate.Manifest,
	}
	workflowTemplate, err := s.resourceManager.CreateWorkflowTemplate(req.Namespace, workflowTemplate)
	if errors.As(err, &userError) {
		return nil, userError.GRPCError()
	}
	req.WorkflowTemplate.Uid = workflowTemplate.UID
	req.WorkflowTemplate.Version = workflowTemplate.Version

	return req.WorkflowTemplate, nil
}

func (s *WorkflowServer) GetWorkflowTemplate(ctx context.Context, req *api.GetWorkflowTemplateRequest) (*api.WorkflowTemplate, error) {
	workflowTemplate, err := s.resourceManager.GetWorkflowTemplate(req.Namespace, req.Uid, req.Version)
	if errors.As(err, &userError) {
		return nil, userError.GRPCError()
	}

	return apiWorkflowTemplate(workflowTemplate), nil
}

func (s *WorkflowServer) ListWorkflowTemplateVersions(ctx context.Context, req *api.ListWorkflowTemplateVersionsRequest) (*api.ListWorkflowTemplateVersionsResponse, error) {
	workflowTemplateVersions, err := s.resourceManager.ListWorkflowTemplateVersions(req.Namespace, req.Uid)
	if errors.As(err, &userError) {
		return nil, userError.GRPCError()
	}

	workflowTemplates := []*api.WorkflowTemplate{}
	for _, wtv := range workflowTemplateVersions {
		workflowTemplates = append(workflowTemplates, apiWorkflowTemplate(wtv))
	}

	return &api.ListWorkflowTemplateVersionsResponse{
		Count:             int32(len(workflowTemplateVersions)),
		WorkflowTemplates: workflowTemplates,
	}, nil
}
