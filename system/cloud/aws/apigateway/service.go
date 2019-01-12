package apigateway

import (
	"fmt"
	"github.com/aws/aws-sdk-go/service/apigateway"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/viant/endly"
	"github.com/viant/endly/system/cloud/aws"
	clambda "github.com/viant/endly/system/cloud/aws/lambda"
	"github.com/viant/toolbox"
	"github.com/viant/toolbox/data"
	"log"
)

const (
	//ServiceID aws api gateway service id.
	ServiceID = "aws/apigateway"
)

//no operation service
type service struct {
	*endly.AbstractService
}

func (s *service) setupResetAPI(context *endly.Context, request *SetupRestAPIInput) (*SetupRestAPIOutput, error) {
	restAPI, resources, err := s.getOrCreateRestAPI(context, request.CreateRestApiInput)
	if err != nil {
		return nil, err
	}
	response := &SetupRestAPIOutput{
		Resources: make([]*SetupResourceOutput, 0),
	}
	response.RestApi = restAPI
	if len(request.Resources) == 0 {
		return response, nil
	}
	var byPath = indexResource(resources.Items)

	for _, resource := range request.Resources {
		parent, ok := byPath[resource.ParentPath()]
		if ! ok {
			available := toolbox.MapKeysToStringSlice(byPath)
			return nil, fmt.Errorf("unable locate parent resource: %v, for part %v,  available: %s", resource.ParentPath(), *resource.PathPart, available)
		}
		resource.ParentId = parent.Id
		resourceOutput, err := s.setupResource(context, resource, restAPI, byPath)
		if err != nil {
			return nil, err
		}
		response.Resources = append(response.Resources, resourceOutput)
	}
	return response, nil
}

func (s *service) getOrCreateRestAPI(context *endly.Context, request *apigateway.CreateRestApiInput) (*apigateway.RestApi, *apigateway.GetResourcesOutput, error) {
	client, err := GetClient(context)
	if err != nil {
		return nil, nil, err
	}
	keysResponse, err := client.GetRestApis(&apigateway.GetRestApisInput{})
	if err != nil {
		return nil, nil, err
	}
	var restAPI *apigateway.RestApi

	for _, item := range keysResponse.Items {
		if *item.Name == *request.Name {
			restAPI = item
			break
		}
	}
	if restAPI == nil {
		if restAPI, err = client.CreateRestApi(request); err != nil {
			return nil, nil, err
		}
	}
	resources, err := client.GetResources(&apigateway.GetResourcesInput{
		RestApiId: restAPI.Id,
	})
	if err != nil || len(resources.Items) == 0 {
		_, err := client.CreateResource(&apigateway.CreateResourceInput{
			RestApiId: restAPI.Id,
		})
		if err != nil {
			return nil, nil, err
		}
		resources, err = client.GetResources(&apigateway.GetResourcesInput{
			RestApiId: restAPI.Id,
		})
	}
	return restAPI, resources, err
}

func (s *service) registerRoutes() {
	client := &apigateway.APIGateway{}
	routes, err := aws.BuildRoutes(client, getClient)
	if err != nil {
		log.Printf("unable register service %v actions: %v\n", ServiceID, err)
		return
	}

	for _, route := range routes {
		route.OnRawRequest = setClient
		s.Register(route)
	}

	s.Register(&endly.Route{
		Action: "setupRestAPI",
		RequestInfo: &endly.ActionInfo{
			Description: "",
		},
		ResponseInfo: &endly.ActionInfo{
			Description: "",
		},
		RequestProvider: func() interface{} {
			return &SetupRestAPIInput{}
		},
		ResponseProvider: func() interface{} {
			return &SetupRestAPIOutput{}
		},
		OnRawRequest: setClient,
		Handler: func(context *endly.Context, request interface{}) (interface{}, error) {
			if req, ok := request.(*SetupRestAPIInput); ok {
				return s.setupResetAPI(context, req)
			}
			return nil, fmt.Errorf("unsupported request type: %T", request)
		},
	})
}

func (s *service) setupResource(context *endly.Context, setup *SetupResourceInput, api *apigateway.RestApi, resources map[string]*apigateway.Resource) (*SetupResourceOutput, error) {
	response := &SetupResourceOutput{
		ResourceMethods:make(map[string]*apigateway.Method),
	}
	resourceInput := setup.CreateResourceInput
	client, err := GetClient(context)
	if err != nil {
		return nil, err
	}
	resource, ok := resources[setup.Path]
	if ! ok {
		resourceInput.RestApiId = api.Id
		if resource, err = client.CreateResource(resourceInput); err != nil {
			return nil, err
		}
		resources[*resource.Path] = resource
	}
	response.Resource = resource
	if err = s.removeUnlistedMethods(client,api,  resource, setup);err != nil {
		return nil, err
	}
	for _, resourceMethod := range setup.Methods {
		method, err := s.setupResourceMethod(context, api, resource, resourceMethod)
		if err != nil {
			return nil, err
		}
		response.ResourceMethods[*method.HttpMethod] = method
	}
	return response, err
}


func (s *service) setupResourceMethod(context *endly.Context, api *apigateway.RestApi, resource *apigateway.Resource, resourceMethod *ResourceMethod) (*apigateway.Method, error) {

	lambdaClient, err := clambda.GetClient(context)
	if err != nil {
		return nil, err
	}
	var state data.Map
	if resourceMethod.Function != "" {
		functionOutput, err := lambdaClient.GetFunction(&lambda.GetFunctionInput{
			FunctionName: &resourceMethod.Function,
		})
		if err != nil {
			return nil, err
		}
		state = buildFunctionState(context, functionOutput.Configuration, api)
		*resourceMethod.Uri = state.ExpandAsText(*resourceMethod.Uri)
	}

	setupMethod := SetupMethodInput(*resourceMethod.PutMethodInput)
	setupMethod.RestApiId = api.Id
	setupMethod.ResourceId = resource.Id
	method, err := s.setupMethod(context, &setupMethod);
	if err != nil {
		return nil, err
	}
	integrationInput := resourceMethod.PutIntegrationInput
	integrationInput.RestApiId = api.Id
	integrationInput.ResourceId = resource.Id
	integrationInput.HttpMethod = setupMethod.HttpMethod
	method.MethodIntegration, err = s.setupMethodIntegration(context, method, resourceMethod.PutIntegrationInput)
	permissionInput := resourceMethod.AddPermissionInput
	if resourceMethod.Function != "" && permissionInput != nil {

		*permissionInput.SourceArn = state.ExpandAsText(*permissionInput.SourceArn)
		*permissionInput.StatementId = state.ExpandAsText(*permissionInput.StatementId)
		request := clambda.SetupPermissionInput(*permissionInput)
		if err = endly.Run(context, &request, nil); err != nil {
			return nil, err
		}
	}
	return method, nil
}



func (s *service) setupMethodIntegration(context *endly.Context, method *apigateway.Method, request *apigateway.PutIntegrationInput) (*apigateway.Integration, error) {
	client, err := GetClient(context)
	if err != nil {
		return nil, err
	}
	if method.MethodIntegration == nil || method.MethodIntegration.Uri == nil {
		return client.PutIntegration(request)
	}
	patchOperations := PutIntegrationInput(*request).Diff(method.MethodIntegration)
	if len(patchOperations) == 0 {
		return method.MethodIntegration, nil
	}
	return client.UpdateIntegration(&apigateway.UpdateIntegrationInput{
		HttpMethod:      request.HttpMethod,
		RestApiId:       request.RestApiId,
		ResourceId:      request.ResourceId,
		PatchOperations: patchOperations,
	})
}

func (s *service) setupMethod(context *endly.Context, request *SetupMethodInput) (*apigateway.Method, error) {
	if request.HttpMethod == nil {
		return nil, nil
	}
	client, err := GetClient(context)
	if err != nil {
		return nil, err
	}
	existingMethod, err := client.GetMethod(&apigateway.GetMethodInput{
		RestApiId:  request.RestApiId,
		ResourceId: request.ResourceId,
		HttpMethod: request.HttpMethod,
	})
	if err != nil || existingMethod == nil {
		putMethod := apigateway.PutMethodInput(*request)
		return client.PutMethod(&putMethod)
	}
	patchOperations := request.Diff(existingMethod)
	if len(patchOperations) == 0 {
		return existingMethod, nil
	}
	return client.UpdateMethod(&apigateway.UpdateMethodInput{
		HttpMethod:      existingMethod.HttpMethod,
		ResourceId:      request.ResourceId,
		RestApiId:       request.RestApiId,
		PatchOperations: patchOperations,
	})
}




func (s *service) removeUnlistedMethods(client *apigateway.APIGateway, api *apigateway.RestApi,  resource *apigateway.Resource, input *SetupResourceInput) error {
	var listedMethods = make(map[string]bool)
	for _, method:= range input.Methods {
		listedMethods[method.HttpMethod] = true
	}
	for k:= range resource.ResourceMethods {
		if _, ok := listedMethods[k]; !ok {
			if _ , err := client.DeleteMethod(&apigateway.DeleteMethodInput{
				HttpMethod:&k,
				ResourceId:resource.Id,
				RestApiId:api.Id,
			});err != nil {
				return err
			}
		}
	}
	return nil
}

//New creates a new AWS API Gateway service.
func New() endly.Service {
	var result = &service{
		AbstractService: endly.NewAbstractService(ServiceID),
	}
	result.AbstractService.Service = result
	result.registerRoutes()
	return result
}