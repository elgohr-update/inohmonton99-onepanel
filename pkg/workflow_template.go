package v1

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/onepanelio/core/pkg/util/env"
	"github.com/onepanelio/core/pkg/util/extensions"
	"github.com/onepanelio/core/pkg/util/ptr"
	"github.com/onepanelio/core/pkg/util/request"
	pagination "github.com/onepanelio/core/pkg/util/request/pagination"
	yaml3 "gopkg.in/yaml.v3"
	"strconv"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	argojson "github.com/argoproj/pkg/json"
	"github.com/ghodss/yaml"
	"github.com/onepanelio/core/pkg/util"
	"github.com/onepanelio/core/pkg/util/label"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkflowTemplateFilter represents the available ways we can filter WorkflowTemplates
type WorkflowTemplateFilter struct {
	Labels []*Label
}

// GetLabels returns the labels in the filter
func (wt *WorkflowTemplateFilter) GetLabels() []*Label {
	return wt.Labels
}

// replaceSysNodePoolOptions replaces a select.nodepool parameter with the nodePool options in the systemConfig
// and returns the new parameters with the change.
func (c *Client) replaceSysNodePoolOptions(parameters []Parameter) (result []Parameter, err error) {
	nodePoolOptions, err := c.systemConfig.NodePoolOptionsAsParameters()
	if err != nil {
		return result, err
	}

	for i := range parameters {
		param := parameters[i]
		if param.Type == "select.nodepool" {
			param.Options = nodePoolOptions

			if param.Value != nil && *param.Value == "default" {
				param.Value = ptr.String(param.Options[0].Value)
			}
		}

		result = append(result, param)
	}

	return
}

// parameterOptionToNodes returns a mapping Node where the content's are the options name/value
func parameterOptionToNodes(option *ParameterOption) *yaml3.Node {
	result := &yaml3.Node{
		Kind: yaml3.MappingNode,
	}

	result.Content = append(result.Content, &yaml3.Node{
		Kind:  yaml3.ScalarNode,
		Value: "name",
	})

	result.Content = append(result.Content, &yaml3.Node{
		Kind:  yaml3.ScalarNode,
		Value: option.Name,
	})

	result.Content = append(result.Content, &yaml3.Node{
		Kind:  yaml3.ScalarNode,
		Value: "value",
	})

	result.Content = append(result.Content, &yaml3.Node{
		Kind:  yaml3.ScalarNode,
		Value: option.Value,
	})

	return result
}

// parameterOptionsToNodes returns an array of nodes representing the options
func parameterOptionsToNodes(options []*ParameterOption) []*yaml3.Node {
	result := make([]*yaml3.Node, 0)

	for _, option := range options {
		result = append(result, parameterOptionToNodes(option))
	}

	return result
}

func applyWorkflowTemplateFilter(sb sq.SelectBuilder, request *request.Request) (sq.SelectBuilder, error) {
	if !request.HasFilter() {
		return sb, nil
	}

	filter, ok := request.Filter.(WorkflowTemplateFilter)
	if !ok {
		return sb, nil
	}

	sb, err := ApplyLabelSelectQuery("wt.labels", sb, &filter)
	if err != nil {
		return sb, err
	}

	return sb, nil
}

// createWorkflowTemplateVersionDB inserts a record into workflow_template_versions using the current time accurate to nanoseconds
// the data is returned in the resulting WorkflowTemplateVersion struct.
func createWorkflowTemplateVersionDB(runner sq.BaseRunner, workflowTemplateVersion *WorkflowTemplateVersion, parameters []Parameter) (err error) {
	if workflowTemplateVersion == nil {
		return fmt.Errorf("workflowTemplateVersion is nil")
	}
	if workflowTemplateVersion.WorkflowTemplate == nil {
		return fmt.Errorf("workflowTemplateVersion.WorkflowTemplate is nil")
	}
	if workflowTemplateVersion.WorkflowTemplate.ID < 1 {
		return fmt.Errorf("workflowTemplateVersion.WorkflowTemplate.ID must be > 0. %v given", workflowTemplateVersion.WorkflowTemplate.ID)
	}

	workflowTemplateVersion.Version = time.Now().UnixNano()

	pj, err := json.Marshal(parameters)
	if err != nil {
		return
	}

	if pj == nil {
		pj = []byte("[]")
	}

	err = sb.Insert("workflow_template_versions").
		SetMap(sq.Eq{
			"workflow_template_id": workflowTemplateVersion.WorkflowTemplate.ID,
			"version":              workflowTemplateVersion.Version,
			"is_latest":            true,
			"manifest":             workflowTemplateVersion.Manifest,
			"parameters":           pj,
			"labels":               workflowTemplateVersion.Labels,
			"description":          workflowTemplateVersion.Description,
		}).
		Suffix("RETURNING id").
		RunWith(runner).
		QueryRow().
		Scan(&workflowTemplateVersion.ID)

	return
}

// updateWorkflowTemplateVersionDB will update a WorkflowTemplateVersion row in the database.
func updateWorkflowTemplateVersionDB(runner sq.BaseRunner, wtv *WorkflowTemplateVersion) (err error) {
	pj, err := json.Marshal(wtv.Parameters)
	if err != nil {
		return
	}
	_, err = sb.Update("workflow_template_versions").
		SetMap(sq.Eq{
			"manifest":   wtv.Manifest,
			"is_latest":  wtv.IsLatest,
			"parameters": string(pj),
		}).
		Where(sq.Eq{
			"id": wtv.ID,
		}).RunWith(runner).Exec()
	return
}

// createLatestWorkflowTemplateVersionDB creates a new workflow template version and marks all previous versions as not latest.
func createLatestWorkflowTemplateVersionDB(runner sq.BaseRunner, workflowTemplateVersion *WorkflowTemplateVersion) (err error) {
	if workflowTemplateVersion == nil {
		return fmt.Errorf("workflowTemplateVersion is nil")
	}
	if workflowTemplateVersion.WorkflowTemplate == nil {
		return fmt.Errorf("workflowTemplateVersion.WorkflowTemplate is nil")
	}
	if workflowTemplateVersion.WorkflowTemplate.ID < 1 {
		return fmt.Errorf("workflowTemplateVersion.WorkflowTemplate.ID must be > 0. %v given", workflowTemplateVersion.WorkflowTemplate.ID)
	}

	_, err = sb.Update("workflow_template_versions").
		Set("is_latest", false).
		Where(sq.Eq{
			"workflow_template_id": workflowTemplateVersion.WorkflowTemplate.ID,
		}).
		RunWith(runner).
		Exec()
	if err != nil {
		return err
	}

	params, err := ParseParametersFromManifest([]byte(workflowTemplateVersion.Manifest))
	if err != nil {
		return err
	}
	return createWorkflowTemplateVersionDB(runner, workflowTemplateVersion, params)
}

// createWorkflowTemplate creates a WorkflowTemplate and all of the DB/Argo/K8s related resources
// The returned WorkflowTemplate has the ArgoWorkflowTemplate set to the newly created one.
func (c *Client) createWorkflowTemplate(namespace string, workflowTemplate *WorkflowTemplate) (*WorkflowTemplate, *WorkflowTemplateVersion, error) {
	if err := workflowTemplate.GenerateUID(workflowTemplate.Name); err != nil {
		return nil, nil, util.NewUserError(codes.InvalidArgument, "Template name must be 30 characters or less")
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	err = sb.Insert("workflow_templates").
		SetMap(sq.Eq{
			"uid":       workflowTemplate.UID,
			"name":      workflowTemplate.Name,
			"namespace": namespace,
			"is_system": workflowTemplate.IsSystem,
			"labels":    workflowTemplate.Labels,
		}).
		Suffix("RETURNING id").
		RunWith(tx).
		QueryRow().
		Scan(&workflowTemplate.ID)
	if err != nil {
		return nil, nil, err
	}

	params, err := ParseParametersFromManifest([]byte(workflowTemplate.Manifest))
	if err != nil {
		return nil, nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	workflowTemplateVersion := &WorkflowTemplateVersion{
		WorkflowTemplate: workflowTemplate,
		Manifest:         workflowTemplate.Manifest,
		Labels:           workflowTemplate.Labels,
	}
	err = createWorkflowTemplateVersionDB(tx, workflowTemplateVersion, params)
	if err != nil {
		return nil, nil, err
	}
	workflowTemplate.WorkflowTemplateVersionID = workflowTemplateVersion.ID

	argoWft, err := createArgoWorkflowTemplate(workflowTemplate, workflowTemplateVersion.Version)
	if err != nil {
		return nil, nil, err
	}
	argoWft.Labels[label.WorkflowTemplateVersionUid] = strconv.FormatInt(workflowTemplateVersion.Version, 10)

	if workflowTemplate.Resource != nil && workflowTemplate.ResourceUID != nil {
		if *workflowTemplate.Resource == TypeWorkspaceTemplate {
			argoWft.Labels[label.WorkspaceTemplateVersionUid] = *workflowTemplate.ResourceUID
		}
	}

	argoWft, err = c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Create(argoWft)
	if err != nil {
		return nil, nil, err
	}

	if err = tx.Commit(); err != nil {
		if errDelete := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Delete(argoWft.Name, &v1.DeleteOptions{}); errDelete != nil {
			err = fmt.Errorf("%w; %s", err, errDelete)
		}
		return nil, nil, err
	}

	workflowTemplate.ArgoWorkflowTemplate = argoWft
	workflowTemplate.Version = workflowTemplateVersion.Version

	return workflowTemplate, workflowTemplateVersion, nil
}

// baseWorkflowTemplatesSelectBuilder returns a SelectBuilder selecting a WorkflowTemplate from the database for a given namespace
// no columns are selected.
func (c *Client) baseWorkflowTemplatesSelectBuilder(namespace string) sq.SelectBuilder {
	sb := sb.Select().
		From("workflow_templates wt").
		Where(sq.Eq{
			"wt.namespace": namespace,
		})

	return sb
}

// workflowTemplatesSelectBuilder returns a SelectBuilder selecting a WorkflowTemplate from the database for a given namespace
// with all of the columns of a WorkflowTemplate
func (c *Client) workflowTemplatesSelectBuilder(namespace string) sq.SelectBuilder {
	sb := c.baseWorkflowTemplatesSelectBuilder(namespace).
		Columns(getWorkflowTemplateColumns("wt")...)

	return sb
}

// countWorkflowTemplateSelectBuilder returns a select builder selecting the total count of WorkflowTemplates
// given the namespace
func (c *Client) countWorkflowTemplateSelectBuilder(namespace string) sq.SelectBuilder {
	sb := c.baseWorkflowTemplatesSelectBuilder(namespace).
		Columns("COUNT(*)")

	return sb
}

// workflowTemplatesVersionSelectBuilder selects data from workflow template versions joined to a workflow template
// the versions/template are filtered by the workflow template's namespace.
func (c *Client) workflowTemplatesVersionSelectBuilder(namespace string) sq.SelectBuilder {
	sb := sb.Select(getWorkflowTemplateVersionColumns("wtv")...).
		From("workflow_template_versions wtv").
		Join("workflow_templates wt ON wt.id = wtv.workflow_template_id").
		Where(sq.Eq{
			"wt.namespace": namespace,
		})

	return sb
}

// workflowTemplateVersionSelectBuilderAll
func (c *Client) workflowTemplateVersionSelectBuilderAll() sq.SelectBuilder {
	sb := sb.Select(getWorkflowTemplateVersionColumns("wtv")...).
		From("workflow_template_versions wtv")
	return sb
}

// GetWorkflowTemplateDB returns a WorkflowTemplate from the database that is not archived, should one exist.
func (c *Client) getWorkflowTemplateDB(namespace, name string) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate = &WorkflowTemplate{}

	sb := c.workflowTemplatesSelectBuilder(namespace).
		Where(sq.Eq{
			"wt.name":        name,
			"wt.is_archived": false,
		})

	err = c.DB.Getx(workflowTemplate, sb)

	return
}

// getWorkflowTemplateVersionDB will return a WorkflowTemplateVersion given the arguments.
// version can be a number as a string, or the string "latest" to get the latest.
func (c *Client) getWorkflowTemplateVersionDB(namespace, name, version string) (workflowTemplateVersion *WorkflowTemplateVersion, err error) {
	workflowTemplateVersion = &WorkflowTemplateVersion{}

	whereMap := sq.Eq{
		"wt.name":        name,
		"wt.is_archived": false,
	}

	if version == "latest" {
		whereMap["wtv.is_latest"] = "true"
	} else {
		whereMap["wtv.version"] = version
	}

	sb := c.workflowTemplatesVersionSelectBuilder(namespace).
		Where(whereMap)

	err = c.DB.Getx(workflowTemplateVersion, sb)

	if err != nil {
		return
	}

	workflowTemplateVersion.Parameters = make([]Parameter, 0)
	if err := json.Unmarshal(workflowTemplateVersion.ParametersBytes, &workflowTemplateVersion.Parameters); err != nil {
		return nil, err
	}

	return
}

// GetLatestWorkflowTemplateVersionDB returns the latest WorkflowTemplateVersion
func (c *Client) getLatestWorkflowTemplateVersionDB(namespace, name string) (workflowTemplateVersion *WorkflowTemplateVersion, err error) {
	return c.getWorkflowTemplateVersionDB(namespace, name, "latest")
}

func (c *Client) getWorkflowTemplateById(id uint64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate = &WorkflowTemplate{}

	query, args, err := sb.Select(getWorkflowTemplateColumns("wt", "")...).
		From("workflow_templates wt").
		Where(sq.Eq{"id": id}).
		ToSql()

	if err != nil {
		return nil, err
	}

	err = c.DB.Get(workflowTemplate, query, args...)

	return
}

func (c *Client) getWorkflowTemplateRaw(namespace, uid string) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate = &WorkflowTemplate{}

	query := sb.Select(getWorkflowTemplateColumns("wt", "")...).
		From("workflow_templates wt").
		Where(sq.Eq{
			"namespace": namespace,
			"uid":       uid,
		})

	err = c.DB.Getx(workflowTemplate, query)

	return
}

// getWorkflowTemplate gets the workflowtemplate given the input data.
// it also loads the argo workflow and labels data.
// If version is <= 0, the latest workflow template is fetched.
// If not found, (nil, nil) is returned
func (c *Client) getWorkflowTemplate(namespace, uid string, version int64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate = &WorkflowTemplate{
		WorkflowExecutionStatisticReport: &WorkflowExecutionStatisticReport{},
	}

	// A new workflow template version is created upon a change, so we use it's created_at
	// as a modified_at for the workflow template.
	sb := c.workflowTemplatesSelectBuilder(namespace).
		Columns("wtv.manifest", "wtv.version", "wtv.id workflow_template_version_id", "wtv.created_at modified_at").
		Join("workflow_template_versions wtv ON wt.id = wtv.workflow_template_id").
		Where(sq.Eq{
			"wt.uid":         uid,
			"wt.is_archived": false,
		})

	if version <= 0 {
		sb = sb.Where(sq.Eq{"wtv.is_latest": true})
	} else {
		sb = sb.Where(sq.Eq{"wtv.version": version})
	}

	if err = c.Getx(workflowTemplate, sb); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}

		return nil, err
	}

	versionAsString := "latest"
	if version != 0 {
		versionAsString = fmt.Sprintf("%v", version)
	}

	argoWft, err := c.getArgoWorkflowTemplate(namespace, uid, versionAsString)
	if err != nil {
		return nil, err
	}
	workflowTemplate.ArgoWorkflowTemplate = argoWft

	templateVersion, err := strconv.ParseInt(argoWft.Labels[label.Version], 10, 64)
	if err != nil {
		return nil, err
	}

	workflowTemplate.Version = templateVersion

	wtv, err := c.getWorkflowTemplateVersionDB(namespace, workflowTemplate.Name, versionAsString)
	if err != nil {
		return workflowTemplate, err
	}

	workflowTemplate.Parameters, err = c.replaceSysNodePoolOptions(wtv.Parameters)
	if err != nil {
		return nil, err
	}

	return workflowTemplate, nil
}

// listWorkflowTemplateVersions grabs WorkflowTemplateVersions and returns them as WorkflowTemplates.
func (c *Client) listWorkflowTemplateVersions(namespace, uid string) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	dbVersions, err := c.selectWorkflowTemplateVersionsDB(namespace, uid)
	if err != nil {
		return nil, err
	}

	for _, version := range dbVersions {
		if version.ParametersBytes != nil {
			_, err := version.LoadParametersFromBytes()
			if err != nil {
				return nil, err
			}

			updatedParameters, err := c.replaceSysNodePoolOptions(version.Parameters)
			if err != nil {
				return nil, err
			}

			version.Parameters = updatedParameters
		}

		newItem := WorkflowTemplate{
			ID:          version.WorkflowTemplate.ID,
			CreatedAt:   version.CreatedAt.UTC(),
			UID:         version.UID,
			Name:        version.WorkflowTemplate.Name,
			Manifest:    version.Manifest,
			Version:     version.Version,
			IsLatest:    version.IsLatest,
			IsArchived:  version.WorkflowTemplate.IsArchived,
			Labels:      version.Labels,
			Parameters:  version.Parameters,
			Description: version.Description,
		}

		workflowTemplateVersions = append(workflowTemplateVersions, &newItem)
	}

	return
}

func (c *Client) selectWorkflowTemplatesQuery(namespace string, request *request.Request) (sb sq.SelectBuilder, err error) {
	sb = c.workflowTemplatesSelectBuilder(namespace).
		Column("COUNT(wtv.*) versions, MAX(wtv.id) workflow_template_version_id").
		Join("workflow_template_versions wtv ON wtv.workflow_template_id = wt.id").
		GroupBy("wt.id", "wt.created_at", "wt.uid", "wt.name", "wt.is_archived").
		OrderBy("wt.created_at DESC")

	sb, err = applyWorkflowTemplateFilter(sb, request)
	if err != nil {
		return
	}

	sb = *request.ApplyPaginationToSelect(&sb)

	return
}

// selectWorkflowTemplatesDB loads non-archived and non-system workflow templates from the database for the input namespace
// it also selects the total number of versions and latest version id
func (c *Client) selectWorkflowTemplatesDB(namespace string, request *request.Request) (workflowTemplates []*WorkflowTemplate, err error) {
	workflowTemplates = make([]*WorkflowTemplate, 0)

	sb, err := c.selectWorkflowTemplatesQuery(namespace, request)
	if err != nil {
		return nil, err
	}

	sb = sb.Where(sq.Eq{
		"wt.is_archived": false,
		"wt.is_system":   false,
	})

	err = c.DB.Selectx(&workflowTemplates, sb)

	return
}

// selectAllWorkflowTemplatesDB loads all workflow templates from the database for the input namespace
// it also selects the total number of versions and latest version id
func (c *Client) selectAllWorkflowTemplatesDB(namespace string, request *request.Request) (workflowTemplates []*WorkflowTemplate, err error) {
	workflowTemplates = make([]*WorkflowTemplate, 0)

	sb, err := c.selectWorkflowTemplatesQuery(namespace, request)
	if err != nil {
		return nil, err
	}

	err = c.DB.Selectx(&workflowTemplates, sb)

	return
}

// CountWorkflowTemplates counts the total number of workflow templates for the given namespace
// archived, and system templates are ignored.
func (c *Client) CountWorkflowTemplates(namespace string, request *request.Request) (count int, err error) {
	sb := sb.Select("COUNT(*)").
		From("workflow_templates wt").
		Where(sq.Eq{
			"wt.namespace":   namespace,
			"wt.is_archived": false,
			"wt.is_system":   false,
		})

	sb, err = applyWorkflowTemplateFilter(sb, request)
	if err != nil {
		return 0, err
	}

	err = sb.RunWith(c.DB).
		QueryRow().
		Scan(&count)

	return
}

func (c *Client) validateWorkflowTemplate(namespace string, workflowTemplate *WorkflowTemplate) (err error) {
	// validate workflow template
	finalBytes, err := workflowTemplate.WrapSpec()
	if err != nil {
		return
	}
	err = c.ValidateWorkflowExecution(namespace, finalBytes)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Workflow could not be validated.")
	}

	return
}

func (c *Client) CreateWorkflowTemplate(namespace string, workflowTemplate *WorkflowTemplate) (*WorkflowTemplate, error) {
	// validate workflow template
	if err := c.validateWorkflowTemplate(namespace, workflowTemplate); err != nil {
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	newWorkflowTemplate, _, err := c.createWorkflowTemplate(namespace, workflowTemplate)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Could not create workflow template.")
		return nil, util.NewUserErrorWrap(err, "Workflow template")
	}

	return newWorkflowTemplate, nil
}

// CreateWorkflowTemplateVersion creates a new workflow template version including argo resources.
// It marks any older workflow template versions as not latest
//
// Pre-condition: a Workflow Template version already exists
// Post-condition: the input workflow template will have it's fields updated so it matches the new version data.
func (c *Client) CreateWorkflowTemplateVersion(namespace string, workflowTemplate *WorkflowTemplate) (*WorkflowTemplate, error) {
	if workflowTemplate.UID == "" {
		return nil, fmt.Errorf("uid required for CreateWorkflowTemplateVersion")
	}

	// validate workflow template
	if err := c.validateWorkflowTemplate(namespace, workflowTemplate); err != nil {
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	wftSb := c.workflowTemplatesSelectBuilder(namespace).
		Where(sq.Eq{
			"wt.uid":         workflowTemplate.UID,
			"wt.is_archived": false,
		})
	workflowTemplateDB := &WorkflowTemplate{}
	if err = c.DB.Getx(workflowTemplateDB, wftSb); err != nil {
		return nil, err
	}

	workflowTemplateVersion := &WorkflowTemplateVersion{
		WorkflowTemplate: workflowTemplateDB,
		Manifest:         workflowTemplate.Manifest,
		Labels:           workflowTemplate.Labels,
		Description:      workflowTemplate.Description,
	}

	err = createLatestWorkflowTemplateVersionDB(tx, workflowTemplateVersion)
	if err != nil {
		return nil, err
	}
	workflowTemplate.WorkflowTemplateVersionID = workflowTemplateVersion.ID

	updatedTemplate, err := createArgoWorkflowTemplate(workflowTemplate, workflowTemplateVersion.Version)
	if err != nil {
		return nil, err
	}

	updatedTemplate.TypeMeta = v1.TypeMeta{}
	updatedTemplate.ObjectMeta.ResourceVersion = ""
	updatedTemplate.ObjectMeta.SetSelfLink("")
	updatedTemplate.Labels[label.WorkflowTemplateVersionUid] = strconv.FormatInt(workflowTemplateVersion.Version, 10)

	parametersMap, err := workflowTemplate.GetParametersKeyString()
	if err != nil {
		return nil, err
	}

	if updatedTemplate.Annotations == nil {
		updatedTemplate.Annotations = make(map[string]string)
	}
	for key, value := range parametersMap {
		updatedTemplate.Annotations[key] = value
	}

	latest, err := c.getArgoWorkflowTemplate(namespace, workflowTemplate.UID, "latest")
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Could not get latest argo workflow template")

		return nil, err
	}
	delete(latest.Labels, label.VersionLatest)

	if _, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Create(updatedTemplate); err != nil {
		return nil, err
	}

	latest, err = c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Update(latest)
	if err != nil {
		return nil, err
	}

	// Make sure the associated workflow template has the latest labels
	_, err = sb.Update("workflow_templates").
		Set("labels", workflowTemplate.Labels).
		Where(sq.Eq{
			"id": workflowTemplateDB.ID,
		}).
		RunWith(tx).
		Exec()
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	workflowTemplate.ID = workflowTemplateDB.ID
	workflowTemplate.Version = workflowTemplateVersion.Version

	return workflowTemplate, nil
}

// UpdateWorkflowTemplateVersion will update a given WorkflowTemplateVersion in the database.
// The intent is to change specific database values for a WorkflowTemplateVersion.
// - wtv.ID has to be set and greater than 0
func (c *Client) UpdateWorkflowTemplateVersion(wtv *WorkflowTemplateVersion) error {
	if wtv.ID <= 0 {
		return fmt.Errorf("id required for UpdateWorkflowTemplateVersionDB")
	}
	return updateWorkflowTemplateVersionDB(c.DB, wtv)
}

// GetWorkflowTemplateRaw returns the WorkflowTemplate without any version information
func (c *Client) GetWorkflowTemplateRaw(namespace, uid string) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate, err = c.getWorkflowTemplateRaw(namespace, uid)
	if err != nil && strings.Contains(err.Error(), "sql: no rows in result set") {
		return nil, nil
	}

	return
}

// GetWorkflowTemplate returns a WorkflowTemplate with data loaded from various sources
// If version is 0, it returns the latest version data.
//
// Data loaded includes
// * Database Information
// * ArgoWorkflowTemplate
// * Labels
func (c *Client) GetWorkflowTemplate(namespace, uid string, version int64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate, err = c.getWorkflowTemplate(namespace, uid, version)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Get Workflow Template failed.")
		return nil, util.NewUserError(codes.Unknown, "Unknown error.")
	}
	if workflowTemplate == nil {
		return nil, util.NewUserError(codes.NotFound, "Workflow template not found.")
	}

	return
}

// GetLatestWorkflowTemplate returns a workflow template with the latest version data.
func (c *Client) GetLatestWorkflowTemplate(namespace, uid string) (workflowTemplate *WorkflowTemplate, err error) {
	return c.GetWorkflowTemplate(namespace, uid, 0)
}

// CountWorkflowTemplatesByName returns the number of WorkflowTemplates given the arguments.
// If archived is nil, it is not considered.
func (c *Client) CountWorkflowTemplatesByName(namespace, name string, archived *bool) (count uint64, err error) {
	sb := c.countWorkflowTemplateSelectBuilder(namespace).
		Where(sq.Eq{"wt.name": name})

	if archived != nil {
		sb = sb.Where(sq.Eq{"wt.is_archived": *archived})
	}

	err = sb.RunWith(c.DB).
		QueryRow().
		Scan(&count)

	return
}

// CountWorkflowTemplateVersions returns the number of versions a non-archived WorkflowTemplate has.
func (c *Client) CountWorkflowTemplateVersions(namespace, uid string) (count uint64, err error) {
	err = sb.Select("COUNT(*)").
		From("workflow_templates wt").
		Join("workflow_template_versions wtv ON wtv.workflow_template_id = wt.id").
		Where(sq.Eq{
			"wt.namespace":   namespace,
			"wt.uid":         uid,
			"wt.is_archived": false,
		}).
		RunWith(c.DB).
		QueryRow().
		Scan(&count)

	return
}

// ListWorkflowTemplateVersions returns all the WorkflowTemplates for a given namespace and uid.
func (c *Client) ListWorkflowTemplateVersions(namespace, uid string) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	workflowTemplateVersions, err = c.listWorkflowTemplateVersions(namespace, uid)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Workflow template versions not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow template versions not found.")
	}

	return
}

// ListWorkflowTemplateVersionsModels returns all the WorkflowTemplateVersions from the database.
// This function is a work-around for ListWorkflowTemplateVersions. Once that function is refactored,
// this function should no longer be necessary and removed.
// See: https://github.com/onepanelio/core/issues/436
func (c *Client) ListWorkflowTemplateVersionsModels(namespace, uid string) (workflowTemplateVersions []*WorkflowTemplateVersion, err error) {
	workflowTemplateVersions, err = c.selectWorkflowTemplateVersionsDB(namespace, uid)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Workflow template versions not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow template versions not found.")
	}
	return
}

// ListWorkflowTemplateVersionsAll returns all WorkflowTemplateVersions with no filtering.
func (c *Client) ListWorkflowTemplateVersionsAll(paginator *pagination.PaginationRequest) (workflowTemplateVersions []*WorkflowTemplateVersion, err error) {
	workflowTemplateVersions, err = c.selectAllWorkflowTemplateVersionsDB(paginator)
	if err != nil {
		log.WithFields(log.Fields{
			"Error": err.Error(),
		}).Error("Workflow template versions not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow template versions not found.")
	}
	return
}

// ListAllWorkflowTemplates lists all of the workflow templates, including archived and system specific
func (c *Client) ListAllWorkflowTemplates(namespace string, request *request.Request) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	workflowTemplateVersions, err = c.selectAllWorkflowTemplatesDB(namespace, request)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Error":     err.Error(),
		}).Error("Workflow templates not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow templates not found.")
	}

	err = c.appendExtraWorkflowTemplateData(namespace, workflowTemplateVersions)

	return
}

// ListWorkflowTemplates returns all WorkflowTemplates where the results
// are filtered by is_archived and is_System is false.
func (c *Client) ListWorkflowTemplates(namespace string, request *request.Request) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	workflowTemplateVersions, err = c.selectWorkflowTemplatesDB(namespace, request)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Error":     err.Error(),
		}).Error("Workflow templates not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow templates not found.")
	}

	err = c.appendExtraWorkflowTemplateData(namespace, workflowTemplateVersions)

	return
}

// appendExtraWorkflowTemplateData adds extra information to workflow templates
// * execution statistics (including cron)
func (c *Client) appendExtraWorkflowTemplateData(namespace string, workflowTemplateVersions []*WorkflowTemplate) (err error) {
	err = c.GetWorkflowExecutionStatisticsForTemplates(workflowTemplateVersions...)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Error":     err.Error(),
		}).Error("Unable to get Workflow Execution Statistic for Templates.")
		return util.NewUserError(codes.NotFound, "Unable to get Workflow Execution Statistic for Templates.")
	}

	err = c.GetCronWorkflowStatisticsForTemplates(workflowTemplateVersions...)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Error":     err.Error(),
		}).Error("Unable to get Cron Workflow Statistic for Templates.")
		return util.NewUserError(codes.NotFound, "Unable to get Cron Workflow Statistic for Templates.")
	}

	return
}

func (c *Client) getLatestWorkflowTemplate(namespace, uid string) (*WorkflowTemplate, error) {
	return c.getWorkflowTemplate(namespace, uid, 0) //version=0 means latest
}

// ArchiveWorkflowTemplate will mark the workflow template identified by the (namespace, uid) as archived
// and will archive or delete resources where appropriate.
func (c *Client) ArchiveWorkflowTemplate(namespace, uid string) (archived bool, err error) {
	workflowTemplate, err := c.getLatestWorkflowTemplate(namespace, uid)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Get Workflow Template failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
	}
	if workflowTemplate == nil {
		return false, util.NewUserError(codes.NotFound, "Workflow template not found.")
	}

	wftVersions, err := c.listWorkflowTemplateVersions(namespace, uid)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Get Workflow Template Versions failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
	}

	//cron workflows
	cronWorkflows := []*CronWorkflow{}
	cwfSB := c.cronWorkflowSelectBuilder(namespace, uid).
		OrderBy("cw.created_at DESC")

	if err := c.DB.Selectx(&cronWorkflows, cwfSB); err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Get Cron Workflows failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
	}
	for _, cwf := range cronWorkflows {
		err = c.ArchiveCronWorkflow(namespace, cwf.Name)
		if err != nil {
			log.WithFields(log.Fields{
				"Namespace": namespace,
				"UID":       uid,
				"Error":     err.Error(),
			}).Error("Archive Cron Workflow failed.")
			return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
		}
	}

	//workflow executions
	paginator := pagination.NewRequest(0, 100)

	for _, wftVer := range wftVersions {
		wfTempVer := strconv.FormatInt(wftVer.Version, 10)
		workflowTemplateName := uid + "-v" + wfTempVer

		for {
			req := &request.Request{Pagination: &paginator}
			wfs, err := c.ListWorkflowExecutions(namespace, uid, wfTempVer, true, req)
			if err != nil {
				log.WithFields(log.Fields{
					"Namespace": namespace,
					"UID":       uid,
					"Error":     err.Error(),
				}).Error("Get Workflow Executions failed.")
				return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
			}
			if len(wfs) == 0 {
				break
			}
			for _, wf := range wfs {
				err = c.ArchiveWorkflowExecution(namespace, wf.UID)
				if err != nil {
					log.WithFields(log.Fields{
						"Namespace": namespace,
						"UID":       uid,
						"Error":     err.Error(),
					}).Error("Archive Workflow Execution Failed.")
					return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
				}
			}
		}

		err = c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Delete(workflowTemplateName, nil)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return true, nil
			}
			return false, err
		}
	}

	_, err = sb.Update("workflow_templates").
		Set("is_archived", true).
		Where(sq.Eq{
			"uid":       uid,
			"namespace": namespace,
		}).RunWith(c.DB).
		Exec()

	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Archive Workflow Template DB failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
	}

	return true, nil
}

// createArgoWorkflowTemplate creates an argo workflow template from the workflowTemplate struct
// the argo template stores the version information.
func createArgoWorkflowTemplate(workflowTemplate *WorkflowTemplate, version int64) (*v1alpha1.WorkflowTemplate, error) {
	var argoWft *v1alpha1.WorkflowTemplate
	var jsonOpts []argojson.JSONOpt
	jsonOpts = append(jsonOpts, argojson.DisallowUnknownFields)

	finalBytes, err := workflowTemplate.WrapSpec()
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(finalBytes, &argoWft)
	if err != nil {
		return nil, err
	}

	if err := workflowTemplate.GenerateUID(workflowTemplate.Name); err != nil {
		return nil, err
	}

	argoWft.Name = fmt.Sprintf("%v-v%v", workflowTemplate.UID, version)

	labels := map[string]string{
		label.WorkflowTemplate:    workflowTemplate.UID,
		label.WorkflowTemplateUid: workflowTemplate.UID,
		label.Version:             fmt.Sprintf("%v", version),
		label.VersionLatest:       "true",
	}

	label.MergeLabelsPrefix(labels, workflowTemplate.Labels, label.TagPrefix)
	argoWft.Labels = labels

	return argoWft, nil
}

// getArgoWorkflowTemplate will load the argo workflow template.
// version "latest" will get the latest version, otherwise a number (as a string) will be used.
func (c *Client) getArgoWorkflowTemplate(namespace, workflowTemplateUID, version string) (*v1alpha1.WorkflowTemplate, error) {
	labelSelect := fmt.Sprintf("%v=%v", label.WorkflowTemplateUid, workflowTemplateUID)
	if version == "latest" {
		labelSelect += "," + label.VersionLatest + "=true"
	} else {
		labelSelect += fmt.Sprintf(",%v=%v", label.Version, version)
	}

	workflowTemplates, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).List(v1.ListOptions{
		LabelSelector: labelSelect,
	})
	if err != nil {
		return nil, err
	}

	templates := workflowTemplates.Items
	if templates.Len() == 0 {
		return nil, errors.New("not found")
	}

	if templates.Len() > 1 {
		return nil, errors.New("not unique result")
	}

	return &templates[0], nil
}

func (c *Client) listArgoWorkflowTemplates(namespace, workflowTemplateUid string) (*[]v1alpha1.WorkflowTemplate, error) {
	labelSelect := fmt.Sprintf("%v=%v", label.WorkflowTemplateUid, workflowTemplateUid)
	workflowTemplates, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).List(v1.ListOptions{
		LabelSelector: labelSelect,
	})
	if err != nil {
		return nil, err
	}

	templates := []v1alpha1.WorkflowTemplate(workflowTemplates.Items)

	return &templates, nil
}

// listDBWorkflowTemplateVersions gets all of the workflow template versions for a specified workflow template uid
// archived ones are ignored. Returned in created_at desc order.
func (c *Client) selectWorkflowTemplateVersionsDB(namespace, workflowTemplateUID string) (versions []*WorkflowTemplateVersion, err error) {
	versions = make([]*WorkflowTemplateVersion, 0)

	sb := c.workflowTemplatesVersionSelectBuilder(namespace).
		Columns(getWorkflowTemplateColumns("wt", "workflow_template")...).
		Where(sq.Eq{
			"wt.uid":         workflowTemplateUID,
			"wt.is_archived": false,
		}).
		OrderBy("wtv.created_at DESC")

	err = c.DB.Selectx(&versions, sb)

	return
}

// selectAllWorkflowTemplateVersionsDB returns all WorkflowTemplateVersions from the database without filter.
func (c *Client) selectAllWorkflowTemplateVersionsDB(paginator *pagination.PaginationRequest) (versions []*WorkflowTemplateVersion, err error) {
	versions = make([]*WorkflowTemplateVersion, 0)

	sb := c.workflowTemplateVersionSelectBuilderAll()
	sb = *paginator.ApplyToSelect(&sb)
	err = c.DB.Selectx(&versions, sb)

	return
}

// prefix is the label prefix.
// e.g. prefix/my-label-key: my-label-value
// if version is 0, latest is used.
func (c *Client) GetWorkflowTemplateLabels(namespace, name, prefix string, version int64) (labels map[string]string, err error) {
	versionAsString := "latest"
	if version != 0 {
		versionAsString = fmt.Sprintf("%v", version)
	}

	wf, err := c.getArgoWorkflowTemplate(namespace, name, versionAsString)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Name":      name,
			"Error":     err.Error(),
		}).Error("Workflow Template not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow Template not found.")
	}

	labels = label.FilterByPrefix(prefix, wf.Labels)
	labels = label.RemovePrefix(prefix, labels)

	return
}

// GenerateWorkflowTemplateManifest replaces any special parameters with runtime values
func (c *Client) GenerateWorkflowTemplateManifest(manifest string) (string, error) {
	root := &yaml3.Node{}
	if err := yaml3.Unmarshal([]byte(manifest), root); err != nil {
		return "", err
	}

	parametersIndex := extensions.CreateYamlIndex("arguments", "parameters")

	if extensions.HasNode(root, parametersIndex) {
		resultNode, err := extensions.GetNode(root, parametersIndex)
		if err != nil {
			return "", err
		}

		nodePoolOptions, err := c.systemConfig.NodePoolOptionsAsParameters()
		if err != nil {
			return "", err
		}
		legalNodePoolValues := []string{env.Get("ONEPANEL_TEMPLATE_NODEPOOL_DEFAULT_VALUE", "default")}
		for _, opt := range nodePoolOptions {
			legalNodePoolValues = append(legalNodePoolValues, opt.Value)
		}

		if len(nodePoolOptions) == 0 {
			return "", fmt.Errorf("no node pool options")
		}

		for _, child := range resultNode.Content {
			hasKey, err := extensions.HasKeyValue(child, "type", "select.nodepool")
			if err != nil {
				return "", err
			}

			if hasKey {
				isValid, err := extensions.HasKeyValue(child, "value", legalNodePoolValues...)
				if err != nil {
					return "", err
				}
				if !isValid {
					return "", util.NewUserError(codes.InvalidArgument, fmt.Sprintf("select.nodepool has an invalid value. Valid values are: %v", strings.Join(legalNodePoolValues, ", ")))
				}

				if err := extensions.SetKeyValue(child, "value", nodePoolOptions[0].Value); err != nil {
					return "", err
				}

				optionsIndex := extensions.CreateYamlIndex("options")

				if extensions.HasNode(child, optionsIndex) {
					return "", util.NewUserError(codes.InvalidArgument, "select.nodepool must not have any options")
				}

				child.Content = append(child.Content, &yaml3.Node{
					Kind:  yaml3.ScalarNode,
					Value: "options",
				}, &yaml3.Node{
					Kind: yaml3.SequenceNode,
				})

				optionsNode, err := extensions.GetNode(child, optionsIndex)
				if err != nil {
					return "", err
				}

				optionsNode.Kind = yaml3.SequenceNode
				optionsNode.Content = parameterOptionsToNodes(nodePoolOptions)
			}
		}
	}

	finalManifest, err := yaml3.Marshal(root)
	if err != nil {
		return "", err
	}

	return string(finalManifest), err
}

// ListWorkflowTemplatesField loads all of the distinct field values for workflow templates
func (c *Client) ListWorkflowTemplatesField(namespace, field string, isSystem bool) (value []string, err error) {
	if field != "name" {
		return nil, fmt.Errorf("unsupported field '%v'", field)
	}

	columnName := fmt.Sprintf("wt.%v", field)

	sb := sb.Select(columnName).
		Distinct().
		From("workflow_templates wt").
		Where(sq.Eq{
			"wt.namespace": namespace,
			"wt.is_system": isSystem,
		}).OrderBy(columnName)

	err = c.DB.Selectx(&value, sb)

	return
}
