package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type responseToolRoute struct {
	Name, Namespace string
	Custom          bool
}

func collectResponseTools(raws []json.RawMessage, namespace string) ([]Tool, map[string]responseToolRoute, error) {
	tools := []Tool{}
	routes := map[string]responseToolRoute{}
	if strings.Count(namespace, ".") > 4 || len(raws) > 256 {
		return nil, nil, fmt.Errorf("too many tool definitions or nested namespaces")
	}
	for _, raw := range raws {
		var tool struct {
			Type              string
			Name, Description string
			Parameters        any
			Strict            bool
			Function          *ToolFunction
			Tools             []json.RawMessage
		}
		if json.Unmarshal(raw, &tool) != nil {
			return nil, nil, fmt.Errorf("invalid tool definition")
		}
		if tool.Name == "" && tool.Function != nil {
			tool.Name = tool.Function.Name
		}
		if tool.Name == "" {
			return nil, nil, fmt.Errorf("Responses tool type %q requires a client tool name; disable unsupported server-side tools in the client", tool.Type)
		}
		fullName := tool.Name
		if namespace != "" {
			fullName = namespace + "." + tool.Name
		}
		if tool.Type == "namespace" {
			nested, nestedRoutes, err := collectResponseTools(tool.Tools, fullName)
			if err != nil {
				return nil, nil, err
			}
			tools = append(tools, nested...)
			for name, route := range nestedRoutes {
				routes[name] = route
			}
			continue
		}
		if tool.Type != "function" && tool.Type != "custom" {
			return nil, nil, fmt.Errorf("unsupported Responses tool type %q; only client function/custom tools are supported", tool.Type)
		}
		fn := ToolFunction{Name: fullName, Description: tool.Description, Parameters: tool.Parameters, Strict: tool.Strict}
		if tool.Function != nil {
			fn = *tool.Function
			fn.Name = fullName
		}
		if tool.Type == "custom" {
			fn.Description += "\nThis is a free-form client tool. Put the exact raw tool input into the required input string argument."
			fn.Parameters = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]string{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
		}
		if _, exists := routes[fullName]; exists {
			return nil, nil, fmt.Errorf("duplicate tool name")
		}
		routes[fullName] = responseToolRoute{Name: tool.Name, Namespace: namespace, Custom: tool.Type == "custom"}
		tools = append(tools, Tool{Type: "function", Function: fn})
	}
	if len(tools) > 256 {
		return nil, nil, fmt.Errorf("too many tools")
	}
	return tools, routes, nil
}

func (p *protocolWriter) customInput(call *outputCall) string {
	var input struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal([]byte(call.Call.Function.Arguments), &input)
	return input.Input
}
func (p *protocolWriter) isCustom(call *outputCall) bool {
	return p.toolRoutes[call.Call.Function.Name].Custom
}
