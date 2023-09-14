// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lambda

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/structure"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/create"
	"github.com/hashicorp/terraform-provider-aws/internal/errs"
	"github.com/hashicorp/terraform-provider-aws/internal/errs/sdkdiag"
	"github.com/hashicorp/terraform-provider-aws/internal/flex"
	"github.com/hashicorp/terraform-provider-aws/internal/tfresource"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
	"github.com/hashicorp/terraform-provider-aws/names"
)

// @SDKResource("aws_lambda_function_environment", name="Manage Lambda Function Environment")
func ResourceFunctionEnvironment() *schema.Resource {
	return &schema.Resource{
		ReadWithoutTimeout:   resourceFunctionEnvironmentRead,
		UpdateWithoutTimeout: resourceFunctionEnvironmentUpdate,
		DeleteWithoutTimeout: resourceFunctionEnvironmentDelete,

		// TIP: ==== TERRAFORM IMPORTING ====
		// If Read can get all the information it needs from the Identifier
		// (i.e., d.Id()), you can use the Passthrough importer. Otherwise,
		// you'll need a custom import function.
		//
		// See more:
		// https://hashicorp.github.io/terraform-provider-aws/add-import-support/
		// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/#implicit-state-passthrough
		// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/#virtual-attributes
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Timeouts: &schema.ResourceTimeout{
			Update: schema.DefaultTimeout(10 * time.Minute),
			Delete: schema.DefaultTimeout(10 * time.Minute),
		},

		// TIP: ==== SCHEMA ====
		// In the schema, add each of the attributes in snake case (e.g.,
		// delete_automated_backups).
		//
		// Formatting rules:
		// * Alphabetize attributes to make them easier to find.
		// * Do not add a blank line between attributes.
		//
		// Attribute basics:
		// * If a user can provide a value ("configure a value") for an
		//   attribute (e.g., instances = 5), we call the attribute an
		//   "argument."
		// * You change the way users interact with attributes using:
		//     - Required
		//     - Optional
		//     - Computed
		// * There are only four valid combinations:
		//
		// 1. Required only - the user must provide a value
		// Required: true,
		//
		// 2. Optional only - the user can configure or omit a value; do not
		//    use Default or DefaultFunc
		// Optional: true,
		//
		// 3. Computed only - the provider can provide a value but the user
		//    cannot, i.e., read-only
		// Computed: true,
		//
		// 4. Optional AND Computed - the provider or user can provide a value;
		//    use this combination if you are using Default or DefaultFunc
		// Optional: true,
		// Computed: true,
		//
		// You will typically find arguments in the input struct
		// (e.g., CreateDBInstanceInput) for the create operation. Sometimes
		// they are only in the input struct (e.g., ModifyDBInstanceInput) for
		// the modify operation.
		//
		// For more about schema options, visit
		// https://pkg.go.dev/github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema#Schema
		Schema: map[string]*schema.Schema{
			"function_arn": {
				Type:     schema.TypeString,
				Computed: true,
			},
			"function_name": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					// Using function name or ARN should not be shown as a diff.
					// Try to convert the old and new values from ARN to function name
					oldFunctionName, oldFunctionNameErr := GetFunctionNameFromARN(old)
					newFunctionName, newFunctionNameErr := GetFunctionNameFromARN(new)
					return (oldFunctionName == new && oldFunctionNameErr == nil) || (newFunctionName == old && newFunctionNameErr == nil)
				},
			},
			"qualifier": {
				Type:         schema.TypeString,
				Optional:     true,
				ForceNew:     true,
				ValidateFunc: validation.NoZeroValues,
			},
			"environment": {
				Type:     schema.TypeList,
				Required: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"variables": {
							Type:     schema.TypeMap,
							Required: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
					},
				},
				// Suppress diff if change is to an empty list
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					if old == "0" && new == "1" {
						_, n := d.GetChange("environment.0.variables")
						newn, ok := n.(map[string]interface{})
						if ok && len(newn) == 0 {
							return true
						}
					}
					return false
				},
			},
		},
	}
}

const (
	ResNameFunctionEnvironment = "Lambda Function Environment"
)

func expandEnvironment(envVars map[string]interface{}) *lambda.Environment {
	if envVars == nil {
		return nil
	}

	env := &lambda.Environment{}

	if v, ok := env["variables"].(*schema.Map); ok && v.Len() > 0 {
		env.Variables = flex.ExpandStringSet(v)
	}

	return env
}

func resourceFunctionEnvironmentCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).LambdaClient(ctx)
	functionName := d.Get("function_name").(string)
	qualifier := d.Get("qualifier").(string)

	id := functionName

	if qualifier != "" {
		id = fmt.Sprintf("%s:%s", functionName, qualifier)
	}

	// TIP: -- 2. Populate a create input structure
	in := &lambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String(functionName),
		Environment: &types.Environment{
			Variables: flex.ExpandStringValueMap(d.Get("environment").([]interface{})[0].(map[string]interface{})["variables"].(map[string]interface{})),
		},
	}

	// TIP: -- 3. Call the AWS update config function
	log.Printf("[DEBUG] Updating lambda function environment: %s", in)
	out, err := conn.UpdateFunctionConfiguration(ctx, in)
	if err != nil {
		return diag.Errorf("failed updating environment for function (%s): %s", id, err)
	}

	// TIP: -- 4. Set the minimum arguments and/or attributes for the Read function to
	// work.
	d.SetId(functionName)

	// TIP: -- 5. Use a waiter to wait for create to complete
	if _, err := waitFunctionEnvironmentCreated(ctx, conn, d.Id(), d.Timeout(schema.TimeoutCreate)); err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionWaitingForCreation, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	// TIP: -- 6. Call the Read function in the Create return
	return append(diags, resourceFunctionEnvironmentRead(ctx, d, meta)...)
}

func resourceFunctionEnvironmentRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	// TIP: ==== RESOURCE READ ====
	// Generally, the Read function should do the following things. Make
	// sure there is a good reason if you don't do one of these.
	//
	// 1. Get a client connection to the relevant service
	// 2. Get the resource from AWS
	// 3. Set ID to empty where resource is not new and not found
	// 4. Set the arguments and attributes
	// 5. Set the tags
	// 6. Return diags

	// TIP: -- 1. Get a client connection to the relevant service
	conn := meta.(*conns.AWSClient).LambdaClient(ctx)

	// TIP: -- 2. Get the resource from AWS using an API Get, List, or Describe-
	// type function, or, better yet, using a finder.
	out, err := findFunctionEnvironmentByID(ctx, conn, d.Id())

	// TIP: -- 3. Set ID to empty where resource is not new and not found
	if !d.IsNewResource() && tfresource.NotFound(err) {
		log.Printf("[WARN] Lambda FunctionEnvironment (%s) not found, removing from state", d.Id())
		d.SetId("")
		return diags
	}

	if err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionReading, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	// TIP: -- 4. Set the arguments and attributes
	//
	// For simple data types (i.e., schema.TypeString, schema.TypeBool,
	// schema.TypeInt, and schema.TypeFloat), a simple Set call (e.g.,
	// d.Set("arn", out.Arn) is sufficient. No error or nil checking is
	// necessary.
	//
	// However, there are some situations where more handling is needed.
	// a. Complex data types (e.g., schema.TypeList, schema.TypeSet)
	// b. Where errorneous diffs occur. For example, a schema.TypeString may be
	//    a JSON. AWS may return the JSON in a slightly different order but it
	//    is equivalent to what is already set. In that case, you may check if
	//    it is equivalent before setting the different JSON.
	d.Set("arn", out.Arn)
	d.Set("name", out.Name)

	// TIP: Setting a complex type.
	// For more information, see:
	// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/#data-handling-and-conversion
	// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/#flatten-functions-for-blocks
	// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/#root-typeset-of-resource-and-aws-list-of-structure
	if err := d.Set("complex_argument", flattenComplexArguments(out.ComplexArguments)); err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionSetting, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	// TIP: Setting a JSON string to avoid errorneous diffs.
	p, err := verify.SecondJSONUnlessEquivalent(d.Get("policy").(string), aws.ToString(out.Policy))
	if err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionSetting, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	p, err = structure.NormalizeJsonString(p)
	if err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionSetting, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	d.Set("policy", p)

	// TIP: -- 6. Return diags
	return diags
}

func resourceFunctionEnvironmentUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	conn := meta.(*conns.AWSClient).LambdaClient(ctx)

	input := &lambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String(d.Id()),
	}

	if d.HasChange("environment") {
		input.Environment = &types.Environment{
			Variables: map[string]string{},
		}

		if v, ok := d.GetOk("environment"); ok && len(v.([]interface{})) > 0 && v.([]interface{})[0] != nil {
			if v, ok := v.([]interface{})[0].(map[string]interface{})["variables"].(map[string]interface{}); ok && len(v) > 0 {
				input.Environment = &types.Environment{
					Variables: flex.ExpandStringValueMap(v),
				}
			}
		}
	}

	_, err := retryFunctionOp(ctx, func() (interface{}, error) {
		return conn.UpdateFunctionConfiguration(ctx, input)
	})

	if err != nil {
		return sdkdiag.AppendErrorf(diags, "updating Lambda Function (%s) environment: %s", d.Id(), err)
	}

	if _, err := waitFunctionEnvironmentUpdated(ctx, conn, d.Id(), d.Timeout(schema.TimeoutUpdate)); err != nil {
		return sdkdiag.AppendErrorf(diags, "waiting for Lambda Function (%s) environment update: %s", d.Id(), err)
	}

	return append(diags, resourceFunctionRead(ctx, d, meta)...)
}

func resourceFunctionEnvironmentDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	// TIP: ==== RESOURCE DELETE ====
	// Most resources have Delete functions. There are rare situations
	// where you might not need a delete:
	// a. The AWS API does not provide a way to delete the resource
	// b. The point of your resource is to perform an action (e.g., reboot a
	//    server) and deleting serves no purpose.
	//
	// The Delete function should do the following things. Make sure there
	// is a good reason if you don't do one of these.
	//
	// 1. Get a client connection to the relevant service
	// 2. Populate a delete input structure
	// 3. Call the AWS delete function
	// 4. Use a waiter to wait for delete to complete
	// 5. Return diags

	// TIP: -- 1. Get a client connection to the relevant service
	conn := meta.(*conns.AWSClient).LambdaClient(ctx)

	// TIP: -- 2. Populate a delete input structure
	log.Printf("[INFO] Deleting Lambda FunctionEnvironment %s", d.Id())

	// TIP: -- 3. Call the AWS delete function
	_, err := conn.DeleteFunctionEnvironment(ctx, &lambda.DeleteFunctionEnvironmentInput{
		Id: aws.String(d.Id()),
	})

	// TIP: On rare occassions, the API returns a not found error after deleting a
	// resource. If that happens, we don't want it to show up as an error.
	if errs.IsA[*types.ResourceNotFoundException](err) {
		return diags
	}
	if err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionDeleting, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	// TIP: -- 4. Use a waiter to wait for delete to complete
	if _, err := waitFunctionEnvironmentDeleted(ctx, conn, d.Id(), d.Timeout(schema.TimeoutDelete)); err != nil {
		return append(diags, create.DiagError(names.Lambda, create.ErrActionWaitingForDeletion, ResNameFunctionEnvironment, d.Id(), err)...)
	}

	// TIP: -- 5. Return diags
	return diags
}

// TIP: ==== STATUS CONSTANTS ====
// Create constants for states and statuses if the service does not
// already have suitable constants. We prefer that you use the constants
// provided in the service if available (e.g., amp.WorkspaceStatusCodeActive).
const (
	statusChangePending = "Pending"
	statusDeleting      = "Deleting"
	statusNormal        = "Normal"
	statusUpdated       = "Updated"
)

// TIP: ==== WAITERS ====
// Some resources of some services have waiters provided by the AWS API.
// Unless they do not work properly, use them rather than defining new ones
// here.
//
// Sometimes we define the wait, status, and find functions in separate
// files, wait.go, status.go, and find.go. Follow the pattern set out in the
// service and define these where it makes the most sense.
//
// If these functions are used in the _test.go file, they will need to be
// exported (i.e., capitalized).
//
// You will need to adjust the parameters and names to fit the service.

// func waitFunctionEnvironmentCreated(ctx context.Context, conn *lambda.Client, id string, timeout time.Duration) (*lambda.Environment, error) {
// 	stateConf := &retry.StateChangeConf{
// 		Pending:                   []string{},
// 		Target:                    []string{statusNormal},
// 		Refresh:                   statusFunctionEnvironment(ctx, conn, id),
// 		Timeout:                   timeout,
// 		NotFoundChecks:            20,
// 		ContinuousTargetOccurence: 2,
// 	}

// 	outputRaw, err := stateConf.WaitForStateContext(ctx)
// 	if out, ok := outputRaw.(*lambda.Environment); ok {
// 		return out, err
// 	}

// 	return nil, err
// }

// TIP: It is easier to determine whether a resource is updated for some
// resources than others. The best case is a status flag that tells you when
// the update has been fully realized. Other times, you can check to see if a
// key resource argument is updated to a new value or not.

func waitFunctionEnvironmentUpdated(ctx context.Context, conn *lambda.Client, id string, timeout time.Duration) (*lambda.Environment, error) {
	stateConf := &retry.StateChangeConf{
		Pending:                   []string{statusChangePending},
		Target:                    []string{statusUpdated},
		Refresh:                   statusFunctionEnvironment(ctx, conn, id),
		Timeout:                   timeout,
		NotFoundChecks:            20,
		ContinuousTargetOccurence: 2,
	}

	outputRaw, err := stateConf.WaitForStateContext(ctx)
	if out, ok := outputRaw.(*lambda.Environment); ok {
		return out, err
	}

	return nil, err
}

// TIP: A deleted waiter is almost like a backwards created waiter. There may
// be additional pending states, however.

func waitFunctionEnvironmentDeleted(ctx context.Context, conn *lambda.Client, id string, timeout time.Duration) (*lambda.Environment, error) {
	stateConf := &retry.StateChangeConf{
		Pending: []string{statusDeleting, statusNormal},
		Target:  []string{},
		Refresh: statusFunctionEnvironment(ctx, conn, id),
		Timeout: timeout,
	}

	outputRaw, err := stateConf.WaitForStateContext(ctx)
	if out, ok := outputRaw.(*lambda.Environment); ok {
		return out, err
	}

	return nil, err
}

// TIP: ==== STATUS ====
// The status function can return an actual status when that field is
// available from the API (e.g., out.Status). Otherwise, you can use custom
// statuses to communicate the states of the resource.
//
// Waiters consume the values returned by status functions. Design status so
// that it can be reused by a create, update, and delete waiter, if possible.

func statusFunctionEnvironment(ctx context.Context, conn *lambda.Client, id string) retry.StateRefreshFunc {
	return func() (interface{}, string, error) {
		out, err := findFunctionEnvironmentByID(ctx, conn, id)
		if tfresource.NotFound(err) {
			return nil, "", nil
		}

		if err != nil {
			return nil, "", err
		}

		return out, aws.ToString(out.Status), nil
	}
}

// TIP: ==== FINDERS ====
// The find function is not strictly necessary. You could do the API
// request from the status function. However, we have found that find often
// comes in handy in other places besides the status function. As a result, it
// is good practice to define it separately.

func findFunctionEnvironmentByID(ctx context.Context, conn *lambda.Client, id string) (*lambda.Environment, error) {
	in := &lambda.GetFunctionEnvironmentInput{
		Id: aws.String(id),
	}
	out, err := conn.GetFunctionEnvironment(ctx, in)
	if errs.IsA[*types.ResourceNotFoundException](err) {
		return nil, &retry.NotFoundError{
			LastError:   err,
			LastRequest: in,
		}
	}
	if err != nil {
		return nil, err
	}

	if out == nil || out.FunctionEnvironment == nil {
		return nil, tfresource.NewEmptyResultError(in)
	}

	return out.FunctionEnvironment, nil
}

// TIP: ==== FLEX ====
// Flatteners and expanders ("flex" functions) help handle complex data
// types. Flatteners take an API data type and return something you can use in
// a d.Set() call. In other words, flatteners translate from AWS -> Terraform.
//
// On the other hand, expanders take a Terraform data structure and return
// something that you can send to the AWS API. In other words, expanders
// translate from Terraform -> AWS.
//
// See more:
// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/
func flattenComplexArgument(apiObject *lambda.ComplexArgument) map[string]interface{} {
	if apiObject == nil {
		return nil
	}

	m := map[string]interface{}{}

	if v := apiObject.SubFieldOne; v != nil {
		m["sub_field_one"] = aws.ToString(v)
	}

	if v := apiObject.SubFieldTwo; v != nil {
		m["sub_field_two"] = aws.ToString(v)
	}

	return m
}

// TIP: Often the AWS API will return a slice of structures in response to a
// request for information. Sometimes you will have set criteria (e.g., the ID)
// that means you'll get back a one-length slice. This plural function works
// brilliantly for that situation too.
func flattenComplexArguments(apiObjects []*lambda.ComplexArgument) []interface{} {
	if len(apiObjects) == 0 {
		return nil
	}

	var l []interface{}

	for _, apiObject := range apiObjects {
		if apiObject == nil {
			continue
		}

		l = append(l, flattenComplexArgument(apiObject))
	}

	return l
}

// TIP: Remember, as mentioned above, expanders take a Terraform data structure
// and return something that you can send to the AWS API. In other words,
// expanders translate from Terraform -> AWS.
//
// See more:
// https://hashicorp.github.io/terraform-provider-aws/data-handling-and-conversion/
func expandComplexArgument(tfMap map[string]interface{}) *lambda.ComplexArgument {
	if tfMap == nil {
		return nil
	}

	a := &lambda.ComplexArgument{}

	if v, ok := tfMap["sub_field_one"].(string); ok && v != "" {
		a.SubFieldOne = aws.String(v)
	}

	if v, ok := tfMap["sub_field_two"].(string); ok && v != "" {
		a.SubFieldTwo = aws.String(v)
	}

	return a
}

// TIP: Even when you have a list with max length of 1, this plural function
// works brilliantly. However, if the AWS API takes a structure rather than a
// slice of structures, you will not need it.
func expandComplexArguments(tfList []interface{}) []*lambda.ComplexArgument {
	// TIP: The AWS API can be picky about whether you send a nil or zero-
	// length for an argument that should be cleared. For example, in some
	// cases, if you send a nil value, the AWS API interprets that as "make no
	// changes" when what you want to say is "remove everything." Sometimes
	// using a zero-length list will cause an error.
	//
	// As a result, here are two options. Usually, option 1, nil, will work as
	// expected, clearing the field. But, test going from something to nothing
	// to make sure it works. If not, try the second option.

	// TIP: Option 1: Returning nil for zero-length list
	if len(tfList) == 0 {
		return nil
	}

	var s []*lambda.ComplexArgument

	// TIP: Option 2: Return zero-length list for zero-length list. If option 1 does
	// not work, after testing going from something to nothing (if that is
	// possible), uncomment out the next line and remove option 1.
	//
	// s := make([]*lambda.ComplexArgument, 0)

	for _, r := range tfList {
		m, ok := r.(map[string]interface{})

		if !ok {
			continue
		}

		a := expandComplexArgument(m)

		if a == nil {
			continue
		}

		s = append(s, a)
	}

	return s
}
