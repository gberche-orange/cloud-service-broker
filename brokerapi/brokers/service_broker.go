// Copyright 2018 the Service Broker Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package brokers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"code.cloudfoundry.org/lager"
	"github.com/pivotal-cf/brokerapi"
	"google.golang.org/api/googleapi"

	"encoding/json"

	"github.com/pivotal/cloud-service-broker/db_service"
	"github.com/pivotal/cloud-service-broker/db_service/models"
	"github.com/pivotal/cloud-service-broker/pkg/credstore"
	"github.com/pivotal/cloud-service-broker/pkg/broker"
)

var (
	invalidUserInputMsg        = "User supplied paramaters must be in the form of a valid JSON map."
	ErrInvalidUserInput        = brokerapi.NewFailureResponse(errors.New(invalidUserInputMsg), http.StatusBadRequest, "parsing-user-request")
	ErrGetInstancesUnsupported = brokerapi.NewFailureResponse(errors.New("the service_instances endpoint is unsupported"), http.StatusBadRequest, "unsupported")
	ErrGetBindingsUnsupported  = brokerapi.NewFailureResponse(errors.New("the service_bindings endpoint is unsupported"), http.StatusBadRequest, "unsupported")
	ErrNonUpdatableParameter   = brokerapi.NewFailureResponse(errors.New("attempt to update parameter that may result in service instance re-creation and data loss"), http.StatusBadRequest, "prohibited")
)

const credhubClientIdentifier = "csb"

// ServiceBroker is a brokerapi.ServiceBroker that can be used to generate an OSB compatible service broker.
type ServiceBroker struct {
	registry  broker.BrokerRegistry
	Credstore credstore.CredStore

	Logger lager.Logger
}

// New creates a ServiceBroker.
// Exactly one of ServiceBroker or error will be nil when returned.
func New(cfg *BrokerConfig, logger lager.Logger) (*ServiceBroker, error) {
	return &ServiceBroker{
		registry:  cfg.Registry,
		Credstore: cfg.Credstore,
		Logger:    logger,
	}, nil
}

// Services lists services in the broker's catalog.
// It is called through the `GET /v2/catalog` endpoint or the `cf marketplace` command.
func (broker *ServiceBroker) Services(ctx context.Context) ([]brokerapi.Service, error) {
	svcs := []brokerapi.Service{}

	enabledServices, err := broker.registry.GetEnabledServices()
	if err != nil {
		return nil, err
	}

	for _, service := range enabledServices {
		entry, err := service.CatalogEntry()
		if err != nil {
			return svcs, err
		}
		svcs = append(svcs, entry.ToPlain())
	}

	return svcs, nil
}

func (broker *ServiceBroker) getDefinitionAndProvider(serviceId string) (*broker.ServiceDefinition, broker.ServiceProvider, error) {
	defn, err := broker.registry.GetServiceById(serviceId)
	if err != nil {
		return nil, nil, err
	}

	providerBuilder := defn.ProviderBuilder(broker.Logger)
	return defn, providerBuilder, nil
}

// Provision creates a new instance of a service.
// It is bound to the `PUT /v2/service_instances/:instance_id` endpoint and can be called using the `cf create-service` command.
func (broker *ServiceBroker) Provision(ctx context.Context, instanceID string, details brokerapi.ProvisionDetails, clientSupportsAsync bool) (brokerapi.ProvisionedServiceSpec, error) {
	broker.Logger.Info("Provisioning", lager.Data{
		"instanceId":         instanceID,
		"accepts_incomplete": clientSupportsAsync,
		"details":            details,
	})

	// make sure that instance hasn't already been provisioned
	exists, err := db_service.ExistsServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("Database error checking for existing instance: %s", err)
	}
	if exists {
		return brokerapi.ProvisionedServiceSpec{}, brokerapi.ErrInstanceAlreadyExists
	}

	brokerService, serviceHelper, err := broker.getDefinitionAndProvider(details.ServiceID)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	// verify the service exists and the plan exists
	plan, err := brokerService.GetPlanById(details.PlanID)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	// verify async provisioning is allowed if it is required
	shouldProvisionAsync := serviceHelper.ProvisionsAsync()
	if shouldProvisionAsync && !clientSupportsAsync {
		return brokerapi.ProvisionedServiceSpec{}, brokerapi.ErrAsyncRequired
	}

	// Give the user a better error message if they give us a bad request
	if !isValidOrEmptyJSON(details.GetRawParameters()) {
		return brokerapi.ProvisionedServiceSpec{}, ErrInvalidUserInput
	}

	// validate parameters meet the service's schema and merge the user vars with
	// the plan's
	vars, err := brokerService.ProvisionVariables(instanceID, details, *plan)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	// get instance details
	instanceDetails, err := serviceHelper.Provision(ctx, vars)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, err
	}

	// save instance details
	instanceDetails.ServiceId = details.ServiceID
	instanceDetails.ID = instanceID
	instanceDetails.PlanId = details.PlanID
	instanceDetails.SpaceGuid = details.SpaceGUID
	instanceDetails.OrganizationGuid = details.OrganizationGUID

	err = db_service.CreateServiceInstanceDetails(ctx, &instanceDetails)
	if err != nil {
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("Error saving instance details to database: %s. WARNING: this instance cannot be deprovisioned through cf. Contact your operator for cleanup", err)
	}

	// save provision request details
	pr := models.ProvisionRequestDetails{
		ServiceInstanceId: instanceID,
		RequestDetails:    string(details.RawParameters),
	}
	if err = db_service.CreateProvisionRequestDetails(ctx, &pr); err != nil {
		return brokerapi.ProvisionedServiceSpec{}, fmt.Errorf("Error saving provision request details to database: %s. Services relying on async provisioning will not be able to complete provisioning", err)
	}

	return brokerapi.ProvisionedServiceSpec{IsAsync: shouldProvisionAsync, DashboardURL: "", OperationData: instanceDetails.OperationId}, nil
}

// Deprovision destroys an existing instance of a service.
// It is bound to the `DELETE /v2/service_instances/:instance_id` endpoint and can be called using the `cf delete-service` command.
// If a deprovision is asynchronous, the returned DeprovisionServiceSpec will contain the operation ID for tracking its progress.
func (broker *ServiceBroker) Deprovision(ctx context.Context, instanceID string, details brokerapi.DeprovisionDetails, clientSupportsAsync bool) (response brokerapi.DeprovisionServiceSpec, err error) {
	broker.Logger.Info("Deprovisioning", lager.Data{
		"instance_id":        instanceID,
		"accepts_incomplete": clientSupportsAsync,
		"details":            details,
	})

	// make sure that instance actually exists
	instance, err := db_service.GetServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return response, brokerapi.ErrInstanceDoesNotExist
	}

	_, serviceProvider, err := broker.getDefinitionAndProvider(instance.ServiceId)
	if err != nil {
		return response, err
	}

	// if async deprovisioning isn't allowed but this service needs it, throw an error
	if serviceProvider.DeprovisionsAsync() && !clientSupportsAsync {
		return response, brokerapi.ErrAsyncRequired
	}

	operationId, err := serviceProvider.Deprovision(ctx, *instance, details)
	if err != nil {
		return response, err
	}

	if operationId == nil {
		// soft-delete instance details from the db if this is a synchronous operation
		// if it's an async operation we can't delete from the db until we're sure delete succeeded, so this is
		// handled internally to LastOperation
		if err := db_service.DeleteServiceInstanceDetailsById(ctx, instanceID); err != nil {
			return response, fmt.Errorf("Error deleting instance details from database: %s. WARNING: this instance will remain visible in cf. Contact your operator for cleanup", err)
		}
		return response, nil
	} else {
		response.IsAsync = true
		response.OperationData = *operationId

		instance.OperationType = models.DeprovisionOperationType
		instance.OperationId = *operationId
		if err := db_service.SaveServiceInstanceDetails(ctx, instance); err != nil {
			return response, fmt.Errorf("Error saving instance details to database: %s. WARNING: this instance will remain visible in cf. Contact your operator for cleanup.", err)
		}
		return response, nil
	}
}

// Bind creates an account with credentials to access an instance of a service.
// It is bound to the `PUT /v2/service_instances/:instance_id/service_bindings/:binding_id` endpoint and can be called using the `cf bind-service` command.
func (broker *ServiceBroker) Bind(ctx context.Context, instanceID, bindingID string, details brokerapi.BindDetails, clientSupportsAsync bool) (brokerapi.Binding, error) {
	broker.Logger.Info("Binding", lager.Data{
		"instance_id": instanceID,
		"binding_id":  bindingID,
		"details":     details,
	})

	// check for existing binding
	exists, err := db_service.ExistsServiceBindingCredentialsByServiceInstanceIdAndBindingId(ctx, instanceID, bindingID)
	if err != nil {
		return brokerapi.Binding{}, fmt.Errorf("Error checking for existing binding: %s", err)
	}
	if exists {
		return brokerapi.Binding{}, brokerapi.ErrBindingAlreadyExists
	}

	// get existing service instance details
	instanceRecord, err := db_service.GetServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return brokerapi.Binding{}, fmt.Errorf("Error retrieving service instance details: %s", err)
	}

	serviceDefinition, serviceProvider, err := broker.getDefinitionAndProvider(instanceRecord.ServiceId)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	// verify the service exists and the plan exists
	plan, err := serviceDefinition.GetPlanById(details.PlanID)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	// Give the user a better error message if they give us a bad request
	if !isValidOrEmptyJSON(details.GetRawParameters()) {
		return brokerapi.Binding{}, ErrInvalidUserInput
	}

	// validate parameters meet the service's schema and merge the plan's vars with
	// the user's
	vars, err := serviceDefinition.BindVariables(*instanceRecord, bindingID, details, plan)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	// create binding
	credsDetails, err := serviceProvider.Bind(ctx, vars)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	serializedCreds, err := json.Marshal(credsDetails)
	if err != nil {
		return brokerapi.Binding{}, fmt.Errorf("Error serializing credentials: %s. WARNING: these credentials cannot be unbound through cf. Please contact your operator for cleanup", err)
	}

	// save binding to database
	newCreds := models.ServiceBindingCredentials{
		ServiceInstanceId: instanceID,
		BindingId:         bindingID,
		ServiceId:         details.ServiceID,
		OtherDetails:      string(serializedCreds),
	}

	if err := db_service.CreateServiceBindingCredentials(ctx, &newCreds); err != nil {
		return brokerapi.Binding{}, fmt.Errorf("Error saving credentials to database: %s. WARNING: these credentials cannot be unbound through cf. Please contact your operator for cleanup",
			err)
	}

	binding, err := serviceProvider.BuildInstanceCredentials(ctx, newCreds, *instanceRecord)
	if err != nil {
		return brokerapi.Binding{}, err
	}

	if broker.Credstore != nil {
		credentialName := getCredentialName(broker.getServiceName(serviceDefinition), bindingID)

		_, err := broker.Credstore.Put(credentialName, binding.Credentials)
		if err != nil {
			return brokerapi.Binding{}, fmt.Errorf("Bind failure: unable to put credentials in Credstore: %v", err)
		}

		_, err = broker.Credstore.AddPermission(credentialName, "mtls-app:"+details.AppGUID, []string{"read"})
		if err != nil {
			return brokerapi.Binding{}, fmt.Errorf("Bind failure: Unable to add Credstore permissions to app: %v", err)
		}

		binding.Credentials = map[string]interface{}{
			"credhub-ref": credentialName,
		}
	}

	return *binding, nil
}

func (broker *ServiceBroker) getServiceName(def *broker.ServiceDefinition) string {
	return def.Name
}

func getCredentialName(serviceName, bindingID string) string {
	return fmt.Sprintf("/c/%s/%s/%s/secrets-and-services", credhubClientIdentifier, serviceName, bindingID)
}

// GetBinding fetches an existing service binding.
// GET /v2/service_instances/{instance_id}/service_bindings/{binding_id}
//
// NOTE: This functionality is not implemented.
func (broker *ServiceBroker) GetBinding(ctx context.Context, instanceID, bindingID string) (brokerapi.GetBindingSpec, error) {
	broker.Logger.Info("GetBinding", lager.Data{
		"instance_id": instanceID,
		"binding_id":  bindingID,
	})

	return brokerapi.GetBindingSpec{}, ErrGetBindingsUnsupported
}

// GetInstance fetches information about a service instance
// GET /v2/service_instances/{instance_id}
//
// NOTE: This functionality is not implemented.
func (broker *ServiceBroker) GetInstance(ctx context.Context, instanceID string) (brokerapi.GetInstanceDetailsSpec, error) {
	broker.Logger.Info("GetInstance", lager.Data{
		"instance_id": instanceID,
	})

	return brokerapi.GetInstanceDetailsSpec{}, ErrGetInstancesUnsupported
}

// LastBindingOperation fetches last operation state for a service binding.
// GET /v2/service_instances/{instance_id}/service_bindings/{binding_id}/last_operation
//
// NOTE: This functionality is not implemented.
func (broker *ServiceBroker) LastBindingOperation(ctx context.Context, instanceID, bindingID string, details brokerapi.PollDetails) (brokerapi.LastOperation, error) {
	broker.Logger.Info("LastBindingOperation", lager.Data{
		"instance_id":    instanceID,
		"binding_id":     bindingID,
		"plan_id":        details.PlanID,
		"service_id":     details.ServiceID,
		"operation_data": details.OperationData,
	})

	return brokerapi.LastOperation{}, brokerapi.ErrAsyncRequired
}

// Unbind destroys an account and credentials with access to an instance of a service.
// It is bound to the `DELETE /v2/service_instances/:instance_id/service_bindings/:binding_id` endpoint and can be called using the `cf unbind-service` command.
func (broker *ServiceBroker) Unbind(ctx context.Context, instanceID, bindingID string, details brokerapi.UnbindDetails, asyncSupported bool) (brokerapi.UnbindSpec, error) {
	broker.Logger.Info("Unbinding", lager.Data{
		"instance_id": instanceID,
		"binding_id":  bindingID,
		"details":     details,
	})

	serviceDefinition, serviceProvider, err := broker.getDefinitionAndProvider(details.ServiceID)
	if err != nil {
		return brokerapi.UnbindSpec{}, err
	}

	// validate existence of binding
	existingBinding, err := db_service.GetServiceBindingCredentialsByServiceInstanceIdAndBindingId(ctx, instanceID, bindingID)
	if err != nil {
		return brokerapi.UnbindSpec{}, brokerapi.ErrBindingDoesNotExist
	}

	// get existing service instance details
	instance, err := db_service.GetServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return brokerapi.UnbindSpec{}, fmt.Errorf("Error retrieving service instance details: %s", err)
	}

	if broker.Credstore != nil {
		credentialName := getCredentialName(broker.getServiceName(serviceDefinition), bindingID)

		err = broker.Credstore.DeletePermission(credentialName)
		if err != nil {
			broker.Logger.Error(fmt.Sprintf("fail to delete permissions on the key %s", credentialName), err)
		}

		err := broker.Credstore.Delete(credentialName)
		if err != nil {
			return  brokerapi.UnbindSpec{}, err
		}
	}

	// remove binding from service provider
	if err := serviceProvider.Unbind(ctx, *instance, *existingBinding); err != nil {
		return brokerapi.UnbindSpec{}, err
	}

	// remove binding from database
	if err := db_service.DeleteServiceBindingCredentials(ctx, existingBinding); err != nil {
		return brokerapi.UnbindSpec{}, fmt.Errorf("Error soft-deleting credentials from database: %s. WARNING: these credentials will remain visible in cf. Contact your operator for cleanup", err)
	}

	return brokerapi.UnbindSpec{}, nil
}

// LastOperation fetches last operation state for a service instance.
// It is bound to the `GET /v2/service_instances/:instance_id/last_operation` endpoint.
// It is called by `cf create-service` or `cf delete-service` if the operation was asynchronous.
func (broker *ServiceBroker) LastOperation(ctx context.Context, instanceID string, details brokerapi.PollDetails) (brokerapi.LastOperation, error) {
	broker.Logger.Info("Last Operation", lager.Data{
		"instance_id":    instanceID,
		"plan_id":        details.PlanID,
		"service_id":     details.ServiceID,
		"operation_data": details.OperationData,
	})

	instance, err := db_service.GetServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return brokerapi.LastOperation{}, brokerapi.ErrInstanceDoesNotExist
	}

	_, serviceProvider, err := broker.getDefinitionAndProvider(instance.ServiceId)
	if err != nil {
		return brokerapi.LastOperation{}, err
	}

	isAsyncService := serviceProvider.ProvisionsAsync() || serviceProvider.DeprovisionsAsync()
	if !isAsyncService {
		return brokerapi.LastOperation{}, brokerapi.ErrAsyncRequired
	}

	lastOperationType := instance.OperationType

	done, err := serviceProvider.PollInstance(ctx, *instance)

	if err != nil {
		// this is a retryable error
		if gerr, ok := err.(*googleapi.Error); ok {
			if gerr.Code == 503 {
				return brokerapi.LastOperation{State: brokerapi.InProgress, Description: err.Error()}, nil
			}
		}

		// This is not a retryable error. Return fail
		return brokerapi.LastOperation{State: brokerapi.Failed, Description: err.Error()}, nil
	}

	if !done {
		return brokerapi.LastOperation{State: brokerapi.InProgress}, nil
	}

	// the instance may have been invalidated, so we pass its primary key rather than the
	// instance directly.
	updateErr := broker.updateStateOnOperationCompletion(ctx, serviceProvider, lastOperationType, instanceID)
	return brokerapi.LastOperation{State: brokerapi.Succeeded}, updateErr
}

// updateStateOnOperationCompletion handles updating/cleaning-up resources that need to be changed
// once lastOperation finishes successfully.
func (broker *ServiceBroker) updateStateOnOperationCompletion(ctx context.Context, service broker.ServiceProvider, lastOperationType, instanceID string) error {
	if lastOperationType == models.DeprovisionOperationType {
		if err := db_service.DeleteServiceInstanceDetailsById(ctx, instanceID); err != nil {
			return fmt.Errorf("Error deleting instance details from database: %s. WARNING: this instance will remain visible in cf. Contact your operator for cleanup", err)
		}

		return nil
	}

	// If the operation was not a delete, clear out the ID and type and update
	// any changed (or finalized) state like IP addresses, selflinks, etc.
	details, err := db_service.GetServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("Error getting instance details from database %v", err)
	}

	if err := service.UpdateInstanceDetails(ctx, details); err != nil {
		return fmt.Errorf("Error getting new instance details from GCP: %v", err)
	}

	details.OperationId = ""
	details.OperationType = models.ClearOperationType
	if err := db_service.SaveServiceInstanceDetails(ctx, details); err != nil {
		return fmt.Errorf("Error saving instance details to database %v", err)
	}

	return nil
}

// Update a service instance plan.
// This functionality is not implemented and will return an error indicating that plan changes are not supported.
func (broker *ServiceBroker) Update(ctx context.Context, instanceID string, details brokerapi.UpdateDetails, asyncAllowed bool) (response brokerapi.UpdateServiceSpec, err error) {
	broker.Logger.Info("Updating", lager.Data{
		"instance_id":        instanceID,
		"accepts_incomplete": asyncAllowed,
		"details":            details,
	})

	// make sure that instance actually exists
	instance, err := db_service.GetServiceInstanceDetailsById(ctx, instanceID)
	if err != nil {
		return response, brokerapi.ErrInstanceDoesNotExist
	}

	brokerService, serviceHelper, err := broker.getDefinitionAndProvider(instance.ServiceId)
	if err != nil {
		return response, err
	}

	// verify the service exists and the plan exists
	plan, err := brokerService.GetPlanById(details.PlanID)
	if err != nil {
		return response, err
	}

	// verify async provisioning is allowed if it is required
	shouldProvisionAsync := serviceHelper.ProvisionsAsync()
	if shouldProvisionAsync && !asyncAllowed {
		return response, brokerapi.ErrAsyncRequired
	}

	// Give the user a better error message if they give us a bad request
	if !isValidOrEmptyJSON(details.GetRawParameters()) {
		return response, ErrInvalidUserInput
	}

	allowUpdate, err := brokerService.AllowedUpdate(details); 

	if err != nil {
		return response, err
	}

	if !allowUpdate {
		return response, ErrNonUpdatableParameter
	}
	
	// validate parameters meet the service's schema and merge the user vars with
	// the plan's
	vars, err := brokerService.UpdateVariables(instanceID, details, *plan)
	if err != nil {
		return response, err
	}

	// get instance details
	newInstanceDetails, err := serviceHelper.Update(ctx, vars)
	if err != nil {
		return brokerapi.UpdateServiceSpec{}, err
	}

	// save instance details

	instance.PlanId = newInstanceDetails.PlanId

	err = db_service.SaveServiceInstanceDetails(ctx, instance)
	if err != nil {
		return brokerapi.UpdateServiceSpec{}, fmt.Errorf("Error saving instance details to database: %s. WARNING: this instance cannot be deprovisioned through cf. Contact your operator for cleanup", err)
	}

	// save provision request details
	pr := models.ProvisionRequestDetails{
		ServiceInstanceId: instanceID,
		RequestDetails:    string(details.RawParameters),
	}
	if err = db_service.SaveProvisionRequestDetails(ctx, &pr); err != nil {
		return brokerapi.UpdateServiceSpec{}, fmt.Errorf("Error saving provision request details to database: %s. Services relying on async provisioning will not be able to complete provisioning", err)
	}

	response.IsAsync = shouldProvisionAsync
	response.DashboardURL = ""
	response.OperationData = newInstanceDetails.OperationId

	return response, nil
}

func isValidOrEmptyJSON(msg json.RawMessage) bool {
	return msg == nil || len(msg) == 0 || json.Valid(msg)
}
