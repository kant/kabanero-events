/*
Copyright 2019 IBM Corporation

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
//	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
	"gopkg.in/yaml.v2"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/interpreter/functions"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog"
)

/* Trigger file syntax

keyword: block, default, if, switch

Settings section:
settrings :
  dryrun: bool

eventTrigger section:
eventTriggers:
  - eventSource: <ident>
   input: <variable>
   body:
    ...
  - eventSources: 
    ...

function section:
functions:
  - name: <ident>
    input: 
      - <ident >
      - <ident>
    output: <ident>
	body:
  - name: <expr>
    ...


And a body:
body:
  - <ident> : <value>
  - <ident>: <expr>
  - if: <condition>
	<ident>: <expr>
  - if: <condition>
	body:
	    - <ident>: <expr>
	    - <ident>: <expr>
  - switch:
	- if: <condition>
	  <ident>: <expr>
	- if: <condition>
	  body:
        - <ident>: <expr>
        - <ident>: <expr>
	- default:
	   - <ident>: <expr>
    - <ident>: <expr>

body:
  - <ident> : <value>
	<ident>: <value>
  - if: <condition>
    <ident>: <value>
  - if: <condition>
    <ident>: <value>
    <ident>: <value>
  - switch:
	- if: <condition>
	  <ident>: <value>
	- if: <condition>
	  body:
        - <ident>: <value>
        - <ident>: <value>
	- <ident>: <value>
*/
  
/* constants for parsing */
const (
	METADATA   = "metadata"
	LABELS     = "labels"
	APIVERSION = "apiVersion"
	KIND       = "kind"
	NAME       = "name"
	NAMESPACE  = "namespace"
	EVENT      = "event" // TODO: remove
	MESSAGE    = "message"
	HEADER     = "header"
    JOBID      = "jobid"
	TYPEINT    = "int"
	TYPEDOUBLE = "double"
	TYPEBOOL   = "bool"
	TYPESTRING = "string"
	TYPELIST   = "list"
	TYPEMAP    = "map"
	WEBHOOK    = "webhook"
	BODY       = "body"
	IF         = "if"
	SWITCH     = "switch"
	DEFAULT    = "default"
	EVENTSOURCE = "eventSource"
	INPUT       = "input"
	SETTINGS    = "settings"
	EVENTTRIGGERS = "eventTriggers"
	FUNCTIONS   = "functions"
)


var keywords map[string] bool = map[string] bool {
	IF: true,
	SWITCH: true,
	DEFAULT: true,
	BODY: true,
}

func  isKeyword(variableName string) bool {
	_, ok := keywords[variableName]
	return ok
}

type eventTriggerDefinition struct {
  setting []map[interface{}]interface{} // all settings 
  eventTriggers map[string] []map[interface{}]interface{} // event source name to triggers 
  functions map[string]map[interface{}]interface{} // funtion name to function body
}

type triggerProcessor struct {
	triggerDef *eventTriggerDefinition
	triggerDir string // directory where trigger file is stored
}

func newTriggerProcessor() *triggerProcessor {
	return &triggerProcessor{}
}

func (tp *triggerProcessor) initialize(fileName string) error {
	if klog.V(6) {
		klog.Infof("triggerProcessor.initialize %v", fileName)
		defer klog.Infof("Leaving trigggerProcessor.initialize %v", fileName)
	}
	var err error
	tp.triggerDef = &eventTriggerDefinition {
		setting: make([]map[interface{}]interface{}, 0),
		eventTriggers: make(map[string] []map[interface{}]interface{}, 0),
		functions: make(map[string]map[interface{}]interface{}, 0),
	}
	err = readTriggerDefinition(fileName, tp.triggerDef)
	if err != nil {
		return err
	}
	tp.triggerDir = filepath.Dir(fileName)
	return nil
}

/* Helper to fetch parameters of trigger object 
  input 
	 tringger: the object containing trigger definition
	 output:
	   eventSource: []string
	   input: string
	   body: []interface{}
*/
func parseTrigger(trigger map[interface{}]interface{}) ( []string, string, []interface{}, error) {
	eventSourceArray := make([]string, 0)
	eventSourceObj, ok  := trigger[EVENTSOURCE]
	if !ok {
		return eventSourceArray, "",  nil, fmt.Errorf("trigger object %v does not containt eventSource", trigger)
	}
	eventSource, ok := eventSourceObj.(string)
	if !ok {
		return eventSourceArray, "",  nil, fmt.Errorf("trigger object %v eventSoruce if not a string but a %T", eventSource, eventSourceObj)

	}
	eventSourceArray = append(eventSourceArray,  eventSource)

	inputObj, ok := trigger[INPUT]
	if !ok {
		return eventSourceArray, "",  nil, fmt.Errorf("trigger object %v does not containt input", trigger)
	}
	input, ok := inputObj.(string)
	if !ok {
		return eventSourceArray, "",  nil, fmt.Errorf("trigger object %v input not string but %T", eventSource, inputObj)

	}

	bodyObj, ok := trigger[BODY]
	if !ok {
		return eventSourceArray, "",  nil, fmt.Errorf("trigger object %v does not containt body", trigger)
	}
	body, ok := bodyObj.([]interface{})
	if !ok {
		return eventSourceArray, "",  nil, fmt.Errorf("trigger object %v body not []interface{} but %T", eventSource, bodyObj)

	}
	return eventSourceArray, input, body, nil
}

func (tp *triggerProcessor) processMessage(message map[string]interface{}, eventSource string ) ([]map[string]interface{}, error) {
	if klog.V(5) {
		klog.Infof("Entering triggerProcessor.processMessage. message: %v, eventSource: %v", message, eventSource);
		defer klog.Infof("Leaving triggerProcessor.processMessage")
	}

	if klog.V(5) {
		klog.Infof("before getting triggerArray")
	}
	triggerArray, ok := tp.triggerDef.eventTriggers[eventSource]
	if !ok {
		err := fmt.Errorf("No trigger found for event source %v", eventSource)
		klog.Error(err)
		return nil, err
	}
	if klog.V(5) {
		klog.Infof("Found triggerArray")
	}

	savedVariables := make([]map[string]interface{}, 0)
	for _, trigger := range(triggerArray) {
		/* evaluate all trigger definitions for the event source*/
		eventSources, inputVariable, bodyArray, err := parseTrigger(trigger)
		if err != nil {
			klog.Error(err)
			return nil, err
		}
		if klog.V(5) {
			klog.Infof("processMessage after parseTrigger: eventsources: %v", eventSources)
		}

		env, variables, err := initializeCELEnv( message, inputVariable)
		if err != nil {
			return nil, err
		}
		if klog.V(5) {
			klog.Infof("processMessage after initializeCELEnv");
		}


		depth := 1
		_,  err = evalBodyArray(env, variables, bodyArray, depth)
		if err != nil {
			klog.Errorf("Error evaluating trigger %v: %v", trigger, err)
			return nil, err
		}
		if klog.V(5) {
			klog.Infof("processMessage after ievalBodyArray");
		}
		savedVariables = append(savedVariables, variables)
	}
	return savedVariables, nil
}

/* Eval body 
   env: the CEL execution environment
   variables: variables gathered so far
   bodyParam: body to evaluate
   depth: depth of recursion
   Return:
	 cel.Env: updated execution environment
	 error: any error
*/
func evalBodyArray(env cel.Env, variables map[string]interface{}, bodyArray []interface{}, depth int) (cel.Env, error ) {

	var err error
	for _, objectObj := range(bodyArray) {
		object, ok := objectObj.(map[interface{}] interface{})
		if !ok {
			return env, fmt.Errorf("body object %v not map[interface{}]interface{}, but of type %T", objectObj, objectObj)
		}
		_, ifOK := object[IF]
		switchObj, switchOK := object[SWITCH]
		if ifOK && switchOK {
			err = fmt.Errorf("object having both if and switch statement not supported. %v", object)
			return env, err
		}

		if ifOK {
			env, _, err := evalIf(env, variables, object, depth)
			if err != nil {
				return env, err
			}
			continue
		}
		if switchOK {
			env, err := evalSwitch(env, variables, switchObj, depth)
			if err != nil {
				return env, err
			}
			continue
		}
		/* just plain assignment */
		env, err = evalAssignment(env, variables, object, depth )
		if err != nil {
			return env, err
		}
	}
	return env, nil
}

func evalAssignment(env cel.Env, variables map[string]interface{}, object map[interface{}]interface{}, depth int) (cel.Env, error) {
	if klog.V(6) {
		klog.Infof("Entering evalAssignment object: %v", object)
		defer klog.Infof("Leaving evalAssignment object")
	}
	var err error
	for variableNameObj, valObj := range object {
		if klog.V(6){
			klog.Infof("processing name: %v, type %T, object: %v, type %T", variableNameObj, variableNameObj, valObj, valObj)
		}
		variableName, ok := variableNameObj.(string)
		if !ok {
			return env, fmt.Errorf("varialbe %s is not  string, but of type %T", variableNameObj, variableNameObj)
		}
		if isKeyword(variableName) {
			continue
		}

		/* Format value as string for CEL parsing */
		var val string
		switch valObj.(type) {
			case int:
				val = fmt.Sprintf("%v", valObj)
			case int64:
				val = fmt.Sprintf("%v", valObj)
			case int32:
				val = fmt.Sprintf("%v", valObj)
			case float32:
				val = fmt.Sprintf("%v", valObj)
			case float64:
				val = fmt.Sprintf("%v", valObj)
			case bool:
				val = fmt.Sprintf("%v", valObj)
			case string:
				val = valObj.(string)
			default:
				return env, fmt.Errorf("Value of variables not stored as  YAML primitive types or string when assgining %v to %v. Type of value is %T", variableName, valObj, valObj)
		}
        env, err = setOneVariable(env, variableName, val, variables ) 
		if err != nil {
			return env, err
		}
	}

	/* check if recursive body exists */
	nestedBodyObj, ok := object[BODY]
	if ok {
		nestedBody, ok := nestedBodyObj.([]interface{})
		if ok {
			return evalBodyArray(env, variables, nestedBody, depth );
		} 
		err = fmt.Errorf("body %v contains nested body that is not array of map[string]interface", nestedBodyObj)
		return env, err
	}
	return env, nil
}

func evalIf(env cel.Env, variables map[string]interface{}, object map[interface{}]interface{}, depth int) (cel.Env, bool, error) {
	conditionObj := object[IF]
	condition, ok := conditionObj.(string)
	if ( !ok ) {
		return env, false, fmt.Errorf("condition of if object not a string: %v", object)
	}
	boolVal, err := evalCondition(env, condition, variables)
	if err != nil {
		return env, false, err
	}

	if !boolVal {
		/* condition not met */
		return env, false, nil
	}
	/* perform assignments */
	env, err = evalAssignment(env, variables, object,  depth)
	return env, true, err
}

func evalSwitch(env cel.Env, variables map[string]interface{}, switchObj interface{}, depth int) (cel.Env, error) {
	var err error
	switchArray , ok := switchObj.([]map[interface{}]interface{})
	if !ok {
		return env, fmt.Errorf("body of switch %v is not []map[interface{}]interface, but %T", switchObj, switchObj)
	}

	defaultObjs := make([]map[interface{}]interface{}, 0)
	for _, object := range(switchArray) {
		_, ifOK := object[IF]
		if ifOK {
			/* evaluate the if statement */
			env, conditionTrue, err := evalIf(env, variables, object, depth)
			if err != nil || conditionTrue {
				return env, err
			}
		} else {
			defaultObjs = append(defaultObjs, object)
		}
	}
	/* evaluate defaults */
	for _, object := range(defaultObjs) {
		env, err = evalAssignment(env, variables, object, depth)
		if err != nil {
			return env, err
		}
	}
	return env, nil
}

//
//	/* Go through the trigger section to find the rule to fire*/
//	action, err := findTrigger(env, tp.triggerDef, variables)
//	if err != nil {
//		return err
//	}
//
//	/* Fire the rule */
//	if action == nil {
//		/* no action found */
//		klog.Infof("No action found for message %s", message)
//		return nil
//	}
//	directory := action.ApplyResources.Directory
//	if directory == "" {
//		klog.Errorf("action drectory in trigger file is empty")
//		return fmt.Errorf("action directory in trigger file is empty")
//	}
//	resourceDir := filepath.Join(tp.triggerDir, directory)
//
//	files := make([] string, 0)
//	err = filepath.Walk(resourceDir, func(path string, info os.FileInfo, err error) error {
//		if err != nil {
//			// problem with accessing a path
//			klog.Errorf("problem processing trigger %s, error %s", path, err)
//			return err
//		}
//		if info.IsDir() {
//			// don't process directory itself
//			klog.Infof("Processing directory %s", path)
//			return nil
//		}
//		if strings.HasSuffix(info.Name(), ".yaml") || strings.HasSuffix(info.Name(), ".yml") {
//			files = append(files, path)
//		} else {
//			klog.Infof("Skipping processing file %s", path)
//		}
//		return nil
//	})
//
//	/* ensure all files are substituted OK*/
//	substituted := make([] string, 0)
//	for _, path := range files {
//		after, err := substituteTemplateFile(path, variables)
//		if err != nil {
//			return err
//		}
//		substituted = append(substituted, after)
//	}
//
//    if tp.triggerDef.Settings != nil && tp.triggerDef.Settings.Dryrun != nil && *tp.triggerDef.Settings.Dryrun {
//		klog.Infof("triggerProcessor: dryrun is set. Resources not created")
//    } else {
//		/* Apply the files */
//		for _, resource:= range substituted {
//			if klog.V(5) {
//				klog.Infof("applying resource: %s", resource)
//			}
//			err = createResource(resource, jobid, dynamicClient)
//			if err != nil {
//				return err
//			}
//		}
//	}
//	return nil

/* Shallow copy a map */
func shallowCopy(originalMap map[string]interface{}) map[string]interface{} {
	newMap := make(map[string]interface{})
	for key, val := range originalMap {
		newMap[key] = val
	}
	return newMap
}

/* Get initial CEL environment
	Return: cel.Env: the CEL environment
		map[string]interface{}: variables used during substitution
		error: any error encountered
 */
func initializeCELEnv(message map[string]interface{}, inputVariableName string) (cel.Env, map[string]interface{},  error) {
	if klog.V(5) {
		klog.Infof("entering initializeCELEnv")
		defer klog.Infof("Leaving initializeCELEnv")
	}
	variables := make(map[string]interface{})

	/* initilize empty CEL environment with additional functions */
	additionalFuncs := getAdditionalCELFuncDecls()
	klog.Infof("Additional Func Decls: %v", additionalFuncs)
	env, err := cel.NewEnv(getAdditionalCELFuncDecls() )
	if err != nil {
		return nil, nil,  err
	}

	ident := decls.NewIdent(inputVariableName, decls.NewMapType(decls.String, decls.Any), nil)
	env, err = env.Extend(cel.Declarations(ident))
	if err != nil {
		return nil, nil,  err
	}
	/* Add message as a new variable */
	variables[inputVariableName] = message

	return env, variables,  nil
}

/* Evaluate one variable and update variables map with any new variables 
	env: the CEL environment
	triggerVar: the variable declaration in the eventTrigger.yaml
	variables: variables mappings collection so far
	Return:
		env: new environment after variables are updated. Even if there are errors, env can be set to contain all valid variables collected up to the point of error
*/
//func evaluateVariable(env cel.Env, triggerVar *Variable,  variables map[string]interface{} ) (cel.Env, error) {
//	/* evaluate value of each variable */
//	if triggerVar == nil {
//		return env, nil
//	}
//	boolVal, err := evalCondition(env, triggerVar.When, variables) 
//	if err != nil {
//		return env, err
//	}
//	/* Condition did not meet */
//	if !boolVal {
//		return env, nil
//	}
//	env, err = setOneVariable(env, triggerVar.Name, triggerVar.Value, variables)
//	if err != nil {
//		return env, err
//	}
//
//	/* recursive evaluate */
//	for _, childVar := range triggerVar.Variables {
//		env, err = evaluateVariable(env, childVar, variables)
//		if err != nil {
//			return env, err
//		}
//	}
//	if klog.V(7) {
//		klog.Infof("After evaluating variable name: %s, value: %s, variables contains: %#v", triggerVar.Name, triggerVar.Value, variables)
//
//	}
//	return env, nil
//}

func setOneVariable(env cel.Env, name string, val string, variables map[string]interface{}) (cel.Env, error) {
	if name == "" {
		/* name not set */
		return env, nil
	}
	
	val = strings.Trim(val, " ")

	parsed, issues := env.Parse(val)
	if issues != nil && issues.Err() != nil {
		return env, fmt.Errorf("Parsing error setting variable %s to %s, error: %v", name, val, issues.Err())
	}
	checked, issues := env.Check(parsed)
	if issues != nil && issues.Err() != nil {
		return env, fmt.Errorf("CEL check error when setting variable %s to %s, error: %v, existing variables: %v", name, val, issues.Err(), variables)
	}
	prg, err := env.Program(checked, getAdditionalCELFuncs())
	if err != nil {
		return env, fmt.Errorf("CEL program error when setting variable %s to %s, error: %v", name, val, err)
	}
	// out, details, err := prg.Eval(variables)
	out, _, err := prg.Eval(variables)
	if err != nil {
		return env, fmt.Errorf("CEL Eval error when setting variable %s to %s, error: %v", name, val, err)
	}

	if klog.V(3) {
		klog.Infof("When setting variable %s to %s, eval of value results in typename: %s, value type: %T, value: %s\n", name, val, out.Type().TypeName(), out.Value(), out.Value())
	}

	env, err = createOneVariable(env, name, val, out, variables)
	return env, err
}


func createOneVariable(env cel.Env, entireName string, val string, out ref.Val, variables map[string]interface{}) (cel.Env, error) {
	if klog.V(6){
		klog.Infof("Entering createOneVariables: setting %v to %v", entireName, val)
		defer klog.Infof("Levaning createOneVariables: setting %v to %v", entireName, val)
	}
	nameArray := strings.Split(entireName, ".")
	arrayLen := len(nameArray)
	tempMap := variables
	var err error
	for index:= 0;  index < arrayLen-1; index++ {
		componentName := nameArray[index]
		entry, ok := tempMap[componentName]
		if !ok {
			/* multi-level name, and entry does not exist. Create it */
			if (index == 0) && (arrayLen > 1) {
				/* create top level identifier */
				ident := decls.NewIdent(componentName, decls.NewMapType(decls.String, decls.Any), nil)
				env, err = env.Extend(cel.Declarations(ident))
				if err != nil {
					return  env, err
				}
			}
			newMap := make(map[string]interface{})
			tempMap[componentName] = newMap
			tempMap = newMap
		} else {
			/* entry already exists */
			entryMap, ok := entry.(map[string]interface{})
			if ! ok {
				return env, fmt.Errorf("unable to set %s to %s: the component name %s already exists but is not a map", entireName, val, componentName) 
			}
			tempMap = entryMap
		}
	}
	lastComponent := nameArray[arrayLen-1]
	/* If we get here, tempMap is a map that we can directly insert the value */
    env, err = createOneVariableHelper(env, entireName, lastComponent,  val, out, tempMap, arrayLen == 1)
	return env, err
}

func createOneVariableHelper(env cel.Env, entireName string, name string, val string, out ref.Val, variables map[string]interface{}, createNewIdent bool) (cel.Env, error) {
	if klog.V(6) {
		klog.Infof("Entering createOneVariableHelper entireName: %v, name: %v, val: %v, type: %v, createNewIdent %v", entireName, name, val, out.Type().TypeName(), createNewIdent)
		defer klog.Infof("Leaving createOneVariableHelper entireName: %v, name: %v, val: %v, type: %v", entireName, name, val, out.Type().TypeName())
	}
	var ok bool
	var err error
	switch out.Type().TypeName() {
	case TYPEINT:
		var intval int64
		if intval, ok = out.Value().(int64); !ok {
			return env, fmt.Errorf("Unable to cast variable %s, value %s into int64", entireName, val)
		}
		floatval := float64(intval)

		if createNewIdent {
			ident := decls.NewIdent(name, decls.Double, nil)
			env, err = env.Extend(cel.Declarations(ident))
			if err != nil {
				return env, err
			}
		}
		variables[name] = floatval
		if klog.V(4) {
			klog.Infof("variable %s set to %v", entireName, floatval)
		}
	case TYPEBOOL:
		boolval, ok := out.Value().(bool);
		if ! ok {
			return env, fmt.Errorf("Unable to cast variable %s, value %s into bool", entireName, val)
		}

		if createNewIdent {
			ident := decls.NewIdent(name, decls.Bool, nil)
			env, err = env.Extend(cel.Declarations(ident))
			if err != nil {
				return env, err
			}
		}
		variables[name] = boolval
		if klog.V(4) {
			klog.Infof("variable %s set to %v", entireName, boolval)
		}
	case TYPEDOUBLE:
		floatvar, ok := out.Value().(float64)
		if !ok {
			return env, fmt.Errorf("Unable to cast variable %s, value %s into float64", entireName, val)
		}

		if createNewIdent {
			ident := decls.NewIdent(name, decls.Double, nil)
			env, err = env.Extend(cel.Declarations(ident))
			if err != nil {
				return env, err
			}
		}
		variables[name]  = floatvar
		if klog.V(4) {
			klog.Infof("variable %s set to %v", entireName, floatvar)
		}
	case TYPESTRING:
		stringval, ok := out.Value().(string)
		if !ok {
			return env, fmt.Errorf("Unable to cast variable %s, value %s into string", entireName, val)
		}

		if createNewIdent {
			ident := decls.NewIdent(name, decls.String, nil)
			env, err = env.Extend(cel.Declarations(ident))
			if err != nil {
				return env, err
			}
		}
		variables[name] = stringval
		if klog.V(4) {
			klog.Infof("variable %s set to %v", entireName, stringval)
		}
	case TYPELIST:
		var listval interface{} = out.Value()
		switch out.Value().(type) {
		case []ref.Val:
		case []interface{}:
		case []string:
		default:
			return  env, fmt.Errorf("Unable to cast variable %s, value %s into %T", entireName, val, out.Value())
		}
		if createNewIdent {
			ident := decls.NewIdent(name, decls.NewListType(decls.Any), nil)
			env, err = env.Extend(cel.Declarations(ident))
			if err != nil {
				return env, err
			}
		}
		variables[name] = listval
		if klog.V(4) {
			klog.Infof("variable %s set to %v", entireName, listval)
		}
	case TYPEMAP:
		mapval, ok := out.Value().(map[string]interface{})
		if !ok {
			_, ok := out.Value().( map[ref.Val]ref.Val)
			if !ok {
				return env, fmt.Errorf("Unable to cast variable %s, value %s, evaluaed value %v, type %T into map[ref.Val]ref.Val", entireName, val, out.Value(), out.Value())
			}
		}
		if createNewIdent {
			ident := decls.NewIdent(name, decls.NewMapType(decls.String, decls.Any), nil)
			env, err = env.Extend(cel.Declarations(ident))
			if err != nil {
				return  env, err
			}
		}
		variables[name] = out.Value()
		if klog.V(4) {
			klog.Infof("variable %s set to %v", entireName, mapval)
		}
	default:
		return env, fmt.Errorf("In function setOneVariable: Unable to process variable name: %s, value: %s, type %s", entireName, val, out.Type().TypeName())
	}
	return env, nil
}

/*
func normalizeMapString(mapObj map[string] interface{}) (map[string]interface{}, error) {
	ret := make(map[string]interface{}, 0)
	for key, val := range yaml {
		newVal, err := normalizeValue(val)
		if err != nil {
			return nil, err
		}
		ret[key] =  newVal
	}
}

func normalizeValue(val interface{}) (interface{}, error)  {
		var newVal interface{}
		var err error
		switch val.(type) {
		case map[interface{}] interface{}:
			newVal, err = normalizeMapInterface(val.(map[interface{}]interface{}))
			if err != nil {
				return nil, err
			}
		case map[string]interface{}:
			newVal, err = normalizeMapString(val.map[string]interface{})
			if err != nil {
				return nil, err
			}
		case [] interface{} :
			newVal, err = normalizeArrayInterface(val.([]interface{}))
			if err != nil {
				return nil, err
			}
		default:
			newVal = val
		}
		return newVal, nil
}

func normalizeMapInterface( mapObj map[interface{}] interface{}) (map[string]interface{}, error) {
	ret := make(map[string]interface{}, 0)
	for key, val := range mapObj {
		keyStr, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("key %v is not string, but %T", key, key)
		}
		ret[keyStr] = val
	}
	return ret, nil
}
func normalizeArrayInterface(val []interface{}) ([]interface{}, error) {
	ret := make([]interface{}, 0)
	for _, valObj := range val {
		newVal, err := normalizeValue(valObj)
		if err != nil {
			return nil, err
		}
		ret = append(ret, newVal)
	}
	return ret, nil
}
*/

func readTriggerDefinition(fileName string, td *eventTriggerDefinition) error {
	if klog.V(5) {
		klog.Infof("enter readTriggerDefinitions %v", fileName)
		defer klog.Infof("Leaving readTriggerDefinitions %v", fileName)
	}
	bytes, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}

	yamlMap := make(map[string]interface{})
	err = yaml.Unmarshal(bytes, yamlMap)
	if err != nil  {
		return fmt.Errorf("Unable to marshal %v. Error: %v", fileName, err)
	}

	/* gather args in the yaml */
	settingsObj, ok := yamlMap[SETTINGS]
	if ok {
		if klog.V(5) {
			klog.Infof("found settings %v", settingsObj)
		}
		settings, ok := settingsObj.(map[interface{}]interface{})
		if ok {
			td.setting = append(td.setting, settings)
		}
	}

	eventTriggersObj, ok := yamlMap[EVENTTRIGGERS]
	if ok {
		if klog.V(5) {
			klog.Infof("found eventTriggers %v %T", eventTriggersObj, eventTriggersObj)
		}
		eventTriggersArray, ok := eventTriggersObj.([]interface{})
		if ok {
			for _, triggerMapObj := range(eventTriggersArray) {
				triggerMap, ok := triggerMapObj.(map[interface{}]interface{})
				if !ok {
					return fmt.Errorf("triggerMapObj %v not type map[interface{}]interface{}, but type %T", triggerMapObj, triggerMapObj)
				}
				eventSourceObj, ok := triggerMap[EVENTSOURCE]
				if klog.V(5) {
					klog.Infof("Found eventSource %v", eventSourceObj)
				}
				if ok {
					eventSource, ok := eventSourceObj.(string)
					if ok {
						existingArray, ok := td.eventTriggers[eventSource]
						if !ok {
							existingArray = make([]map[interface{}]interface{}, 0)
						}
						td.eventTriggers[eventSource] = append(existingArray, triggerMap)
					}
				}
			}
		} else {
			return fmt.Errorf("event trigger %v not type map[interface{}]interface{] but type %F", eventTriggersObj, eventTriggersObj)
		}
	}

	/* read functions */
	functionsObj, ok := yamlMap[FUNCTIONS]
	if ok {
		functionsArray, ok := functionsObj.([]interface{})
		if ok {
			for _, functionMapObj := range(functionsArray) {
				functionMap, ok := functionMapObj.(map[interface{}] interface{})
				if !ok {
					if klog.V(5) {
						klog.Infof("functionMap %v not of type map[interface{}]interface{}, but is type %T", functionMapObj, functionMapObj)
					}
					continue
				}
				nameObj, ok := functionMap[NAME]
				if ok {
					name, ok := nameObj.(string)
					if ok {
						td.functions[name] = functionMap
					}
				}
			}
		} else {
			return fmt.Errorf("functionsArray %v not of type []interface{}, but type %T", functionsArray, functionsArray)
		}
	} 

	return nil
}

/* Find Action for trigger. Only return the first one found*/
//func findTrigger(env cel.Env, td *EventTriggerDefinition, variables map[string]interface{}) (*Action, error) {
//	if td.EventTriggers == nil {
//		return nil, nil
//	}
//	for _, trigger := range td.EventTriggers {
//		action, err := evalTrigger(env, trigger, variables)
//		if action != nil || err != nil {
//			// either found a match or error
//			return action, err
//		}
//	}
//	return nil, nil
//}

func evalCondition(env cel.Env, when string, variables map[string]interface{}) (bool, error) {
	if when == "" {
		/* unconditional */
		return true, nil
	}
	parsed, issues := env.Parse(when)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("Error evaluating condition %s, error: %v", when, issues.Err())
	}
	checked, issues := env.Check(parsed)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("Error parsing condition %s, error: %v", when, issues.Err())
	}
	prg, err := env.Program(checked, getAdditionalCELFuncs())
	if err != nil {
		return false, fmt.Errorf("Error creating CEL program for condition %s, error: %v", when, err)
	}
	// out, details, err := prg.Eval(variables)
	out, _, err := prg.Eval(variables)
	if err != nil {
		return false, fmt.Errorf("Error evaluating condition %s, error: %v", when, err)
	}

	var boolVal bool
	var ok bool
	if boolVal, ok = out.Value().(bool); !ok {
		return false, fmt.Errorf("The when expression %s does not evaluate to a bool. Instead it is of type %T", when, out.Value())
	}
	return boolVal, nil
}

//func evalTrigger(env cel.Env, trigger *EventTrigger, variables map[string]interface{}) (*Action, error) {
//	if trigger == nil {
//		return nil, nil
//	}
//
//	boolVal, err := evalCondition(env, trigger.When, variables)
//	if err != nil {
//		return nil, err
//	}
//
//	if boolVal {
//		/* trigger matched */
//		klog.Infof("Trigger %s matched", trigger.When)
//		if trigger.Action != nil {
//			return trigger.Action, nil
//		}
//
//		/* no action. Check children to see if they match */
//		if trigger.EventTriggers == nil {
//			return nil, nil
//		}
//
//		for _, trigger := range trigger.EventTriggers {
//			action, err := evalTrigger(env, trigger, variables)
//			if action != nil || err != nil {
//				// either found a match or error
//				return action, err
//			}
//		}
//	}
//	return nil, nil
//}

func substituteTemplateFile(fileName string, variables map[string]interface{}) (string, error) {
	bytes, err := ioutil.ReadFile(fileName)
	if err != nil {
		return "", err
	}
	str := string(bytes)
	klog.Infof("Before template substitution for %s: %s", fileName, str)
	substituted, err := substituteTemplate(str, variables)
	if err != nil {
		klog.Errorf("Error in template substitution for %s: %s", fileName, err)
	} else {
		klog.Infof("After template substitution for %s: %s", fileName, substituted)
	}
	return substituted, err
}

func substituteTemplate(templateStr string, variables map[string]interface{}) (string, error) {
	t, err := template.New("kabanero").Parse(templateStr)
	if err != nil {
		return "", err
	}
	buffer := new(bytes.Buffer)
	err = t.Execute(buffer, variables)
	if err != nil {
		return "", err
	}
	return buffer.String(), nil
}

/* Create application. Assume it does not already exist */
func createResource(resourceStr string, jobid string, dynamicClient dynamic.Interface) error {
	if klog.V(4) {
		klog.Infof("Creating resource %s", resourceStr)
	}

	/* Convert yaml to unstructured*/
	resourceBytes, err := k8syaml.ToJSON([]byte(resourceStr))
	if err != nil {
		return fmt.Errorf("Unable to convert yaml resource to JSON: %v", resourceStr)
	}
	var unstructuredObj = &unstructured.Unstructured{}
	err = unstructuredObj.UnmarshalJSON(resourceBytes)
	if err != nil {
		klog.Errorf("Unable to convert JSON %s to unstructured", resourceStr)
		return err
	}

	group, version, resource, namespace, name, err := getGroupVersionResourceNamespaceName(unstructuredObj)
	if namespace == "" {
		return fmt.Errorf("resource %s does not contain namepsace", resourceStr)
	}

	/* add labale kabanero.io/jobld = <jobid> */
	err = setJobID(unstructuredObj, jobid)
	if err != nil {
		return err
	}

	if klog.V(5) {
		klog.Infof("Resource after adding jobid: %v", unstructuredObj)
	}
	 gvr := schema.GroupVersionResource{group, version, resource}
	if err == nil {
		var intfNoNS = dynamicClient.Resource(gvr)
		var intf dynamic.ResourceInterface
		intf = intfNoNS.Namespace(namespace)

		_, err = intf.Create(unstructuredObj, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Unable to create resource %s/%s error: %s", namespace, name, err)
			return err
		}
	} else {
		klog.Errorf("Unable to create resource /%s.  Error: %s", resourceStr, err)
		return fmt.Errorf("Unable to get GVR for resource %s, error: %s", resourceStr, err)
	}
	if klog.V(2) {
		klog.Infof("Created resource %s/%s", namespace, name)
	}
	return nil
}

func setJobID(unstructuredObj *unstructured.Unstructured, jobid string) error {
	var objMap = unstructuredObj.Object
	metadataObj, ok := objMap[METADATA]
	if !ok {
		return fmt.Errorf("Resource has no metadata: %v", unstructuredObj)
	}
	var metadata map[string]interface{}
	metadata, ok = metadataObj.(map[string]interface{})
	if !ok {
		return fmt.Errorf("Resource metadata is not a map: %v", unstructuredObj)
	}

	labelsObj, ok := metadata[LABELS]
	var labels map[string]interface{}
	if !ok {
		/* create new label */
		labels = make(map[string]interface{})
		metadata[LABELS] = labels
	} else {
		labels, ok = labelsObj.(map[string]interface{})
		if !ok {
			return fmt.Errorf("Resource labels is not a map: %v", labels)
		}
	}
	labels["kabanero.io/jobid"] = jobid
	
	return nil
}


/* Return GVR, namespace, name, ok */
func getGroupVersionResourceNamespaceName(unstructuredObj *unstructured.Unstructured) (string, string, string, string, string, error) {
	var objMap = unstructuredObj.Object
	apiVersionObj, ok := objMap[APIVERSION]
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource has does not contain apiVersion: %s", unstructuredObj)
	}
	apiVersion, ok := apiVersionObj.(string)
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource apiVersion not a string: %s", unstructuredObj)
	}

	components := strings.Split(apiVersion, "/")
	var group, version string
	if len(components) == 1 {
		group = ""
		version = components[0]
	} else if len(components) == 2 {
		group = components[0]
		version = components[1]
	} else {
		return "", "", "", "", "", fmt.Errorf("Resource has invalid group/version: %s, resource: %s", apiVersion, unstructuredObj)
	}

	kindObj, ok := objMap[KIND]
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource has invalid kind: %s", unstructuredObj)
	}
	kind, ok := kindObj.(string)
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource kind not a string: %s", unstructuredObj)
	}
	resource := kindToPlural(kind)

	metadataObj, ok := objMap[METADATA]
	var metadata map[string]interface{}
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource has no metadata: %s", unstructuredObj)
	}
	metadata, ok = metadataObj.(map[string]interface{})
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource metadata is not a map: %s", unstructuredObj)
	}

	nameObj, ok := metadata[NAME]
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource has no name: %s", unstructuredObj)
	}
	name, ok := nameObj.(string)
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource name not a string: %s", unstructuredObj)
	}

	nsObj, ok := metadata[NAMESPACE]
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource has no namespace: %s", unstructuredObj)
	}
	namespace, ok := nsObj.(string)
	if !ok {
		return "", "", "", "", "", fmt.Errorf("Resource namespace not a string: %s", unstructuredObj)
	}
	return group, version, resource, namespace, name, nil
}

func kindToPlural(kind string) string {
	lowerKind := strings.ToLower(kind)
	if index := strings.LastIndex(lowerKind, "ss"); index == len(lowerKind)-2 {
		return lowerKind + "es"
	}
	if index := strings.LastIndex(lowerKind, "cy"); index == len(lowerKind)-2 {
		return lowerKind[0:index] + "cies"
	}
	return lowerKind + "s"
}

/* tracking time for timestamp */
var lastTime time.Time = time.Now().UTC()
var mutex = &sync.Mutex{}
/* 
Get timestamp. Timestamp format is UTC time expressed as:
      YYYYMMDDHHMMSSL, where L is last digits in multiples of 1/10 second.
WARNING: This function may sleep up to 0.1 second per request if there are too many concurent requests
*/
func getTimestamp() string {
	mutex.Lock()
	defer mutex.Unlock()

	now := time.Now().UTC()
	for now.Sub(lastTime).Nanoseconds() < 100000000 {
            // < 0.1 since last time. Sleep for at least 0.1 second
            time.Sleep(time.Millisecond*time.Duration(100))
            now = time.Now().UTC()
	}
	lastTime = now

	return fmt.Sprintf("%04d%02d%02d%02d%02d%02d%01d", now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(),now.Second(), now.Nanosecond()/100000000)
}

/* implementation of toDomainName for CEL */
func toDomainNameCEL(param ref.Val) ref.Val {
	str, ok := param.(types.String)
	if !ok {
		return types.ValOrErr(param, "unexpected type '%v' passed to toDomainName", param.Type())
	}
	return types.String(toDomainName(string(str)))
}

/* implementation of toLabel for CEL */
func toLabelCEL(param ref.Val) ref.Val {
	str, ok := param.(types.String)
	if !ok {
		return types.ValOrErr(param, "unexpected type '%v' passed to toLabel", param.Type())
	}
	return types.String(toLabel(string(str)))
}

/* implementation of split for CEL */
func splitCEL(strVal ref.Val, sepVal ref.Val ) ref.Val {
	str, ok := strVal.(types.String)
	if !ok {
		return types.ValOrErr(strVal, "unexpected type '%v' passed as first parameter to function split", strVal.Type())
	}
	sep, ok := sepVal.(types.String)
	if !ok {
		return types.ValOrErr(sepVal, "unexpected type '%v' passed as second parameter to function split", sepVal.Type())
	}
	arrayStr := strings.Split(string(str), string(sep))

	return types.NewStringList(types.DefaultTypeAdapter, arrayStr)
}

/* Return next job ID */
func jobIDCEL(values ...ref.Val) ref.Val {
	return types.String(getTimestamp())
}


/* Return next job ID */
func kabaneroConfigCEL(values ...ref.Val) ref.Val {
    ret := make(map[string]interface{})
	ret[NAMESPACE] = webhookNamespace
	return types.NewDynamicMap(types.DefaultTypeAdapter, ret)
}

/* implementation of downlodYAML for CEL. 
   webhookMessage: map[string]interface{} contains the original webhook message
   fileNameVal: name of file to download
   Return: map[string] interface{} where
	   map["error"], if set, is the error message enccountered when reading the file.
       map["exists"] is true if the file exists, or false if it doesn't exist
	   map["content"], if set, is the actual file content, of type map[string]interface{}
*/
func downloadYAMLCEL(webhookMessage ref.Val, fileNameVal ref.Val) ref.Val {
	klog.Infof("downloadYAMLCEL first param: %v, second param: %v", webhookMessage, fileNameVal)

	if webhookMessage.Value() == nil {
		return types.ValOrErr(webhookMessage, "unexpected null first parameter passed to function downloadYAML.") 
	}
	mapInst, ok := webhookMessage.Value().(map[string]interface{})
	if !ok {
		return types.ValOrErr(webhookMessage, "unexpected type '%v' passed as first parameter to function downloadYAML. It should be map[string]interface{}", webhookMessage.Type())
	}

	bodyMapObj, ok := mapInst[BODY]
	if !ok {
		return types.ValOrErr(webhookMessage, "Missing event parameter %v passed to downloadYAML.", webhookMessage)
	}
	var bodyMap map[string]interface{}
	bodyMap, ok  = bodyMapObj.(map[string]interface{})
	if !ok {
		return types.ValOrErr(webhookMessage, "Event parameter %v passed to downloadYAML not map[string]interface{}. Instead, it is %T.", webhookMessage, bodyMapObj)
	}

	headerMapObj, ok := mapInst[HEADER]
	if !ok {
		return types.ValOrErr(webhookMessage, "Missing header parameter %v passed to downloadYAML.", webhookMessage)
	}
	var headerMap map[string][]string
	headerMap, ok = headerMapObj.(map[string][]string)
	if !ok {
		return types.ValOrErr(webhookMessage, "Parameter %v passed to downloadYAML not map[string][]string. Instead, it is %T.", headerMapObj, headerMapObj)
	}
	
	if fileNameVal.Value() == nil {
		return types.ValOrErr(fileNameVal, "unexpected null second parameter passed to function downloadYAML.") 
	}
	fileName, ok := fileNameVal.Value().(string)
	if !ok {
		return types.ValOrErr(fileNameVal, "unexpected type '%v' passed as first parameter to function downloadYAML. It should be string", fileNameVal.Type())
	}

	var ret map[string]interface{} = make(map[string]interface{})
	fileContent, exists , err := downloadYAML(headerMap, bodyMap, fileName) 
	ret["exists"] = exists
	if err != nil {
		ret["error"] = fmt.Sprintf("%v", err)
		if klog.V(5) {
			klog.Infof("downloadYAMLCEI error: %v", err)
		}
	} else {
		ret["content"] = fileContent
		if klog.V(5) {
			klog.Infof("downloadYAMLCEI content: %v", fileContent)
		}
	}
	return types.NewDynamicMap(types.DefaultTypeAdapter, ret)
}

/* Get declaration of additional overloaded CEL functions */
func getAdditionalCELFuncDecls() cel.EnvOption{
	return triggerFuncDecls
}

/* Get implemenations of additional overloaded CEL functions */
func getAdditionalCELFuncs() cel.ProgramOption {
	return triggerFuncs
}

var triggerFuncDecls cel.EnvOption
var triggerFuncs cel.ProgramOption

func init() {
	triggerFuncDecls = cel.Declarations (
		decls.NewFunction("kabaneroConfig", 
			decls.NewOverload("kabaneroConfig", []*exprpb.Type{}, decls.NewMapType(decls.String, decls.Any))),
		decls.NewFunction("jobID", 
			decls.NewOverload("jobID", []*exprpb.Type{}, decls.String)),
		decls.NewFunction("downloadYAML", 
			decls.NewOverload("downloadYAML_map_string", []*exprpb.Type{decls.NewMapType(decls.String, decls.Any), decls.String}, decls.NewMapType(decls.String, decls.Any))),
		decls.NewFunction("toDomainName", 
			decls.NewOverload("toDomainName_string", []*exprpb.Type{decls.String}, decls.String)),
		decls.NewFunction("toLabel", 
			decls.NewOverload("toLabel_string", []*exprpb.Type{decls.String}, decls.String)),
		decls.NewFunction("split",
			decls.NewOverload("split_string", []*exprpb.Type{decls.String, decls.String}, decls.NewListType(decls.String))))

	triggerFuncs = cel.Functions(
		&functions.Overload{
	        Operator: "kabaneroConfig",
	        Function: kabaneroConfigCEL} ,
		&functions.Overload{
	        Operator: "jobID",
	        Function: jobIDCEL} ,
		&functions.Overload{
	        Operator: "downloadYAML",
	        Binary: downloadYAMLCEL} ,
		&functions.Overload{
	        Operator: "toDomainName",
	        Unary: toDomainNameCEL} ,
		&functions.Overload{
	        Operator: "toLabel",
	        Unary: toLabelCEL} ,
		&functions.Overload{
	        Operator: "split",
	        Binary: splitCEL})
}
