// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/terraform/internal/actions"
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

type nodeActionApply struct {
	TriggeringResourceaddrs addrs.AbsResourceInstance
	ActionInvocations       []*plans.ActionInvocationInstance
}

var (
	_ GraphNodeExecutable      = (*nodeActionApply)(nil)
	_ GraphNodeReferencer      = (*nodeActionApply)(nil)
	_ dag.GraphNodeDotter      = (*nodeActionApply)(nil)
	_ GraphNodeActionProviders = (*nodeActionApply)(nil)
)

func (n *nodeActionApply) Name() string {
	return fmt.Sprintf("%s after actions", n.TriggeringResourceaddrs)
}

func (n *nodeActionApply) DotNode(string, *dag.DotOpts) *dag.DotNode {
	return &dag.DotNode{
		Name: n.Name(),
	}
}

func (n *nodeActionApply) Execute(ctx EvalContext, _ walkOperation) tfdiags.Diagnostics {
	return invokeActions(ctx, n.TriggeringResourceaddrs, n.ActionInvocations)
}

func invokeActions(ctx EvalContext, triggeringResourceAddrs addrs.AbsResourceInstance, actionInvocations []*plans.ActionInvocationInstance) tfdiags.Diagnostics {
	finishedActionInvocations, diags := processActionInvocations(ctx, actionInvocations)
	return betterDiags(triggeringResourceAddrs, finishedActionInvocations, actionInvocations, diags)
}

func processActionInvocations(ctx EvalContext, actionInvocations []*plans.ActionInvocationInstance) ([]*plans.ActionInvocationInstance, tfdiags.Diagnostics) {
	var finishedActionInvocations []*plans.ActionInvocationInstance
	var diags tfdiags.Diagnostics
	// First we order the action invocations by their trigger block index and events list index.
	// This way we have the correct order of execution.
	orderedActionInvocations := make([]*plans.ActionInvocationInstance, len(actionInvocations))
	copy(orderedActionInvocations, actionInvocations)
	sort.Slice(orderedActionInvocations, func(i, j int) bool {
		if orderedActionInvocations[i].ActionTriggerBlockIndex == orderedActionInvocations[j].ActionTriggerBlockIndex {
			return orderedActionInvocations[i].ActionsListIndex < orderedActionInvocations[j].ActionsListIndex
		}
		return orderedActionInvocations[i].ActionTriggerBlockIndex < orderedActionInvocations[j].ActionTriggerBlockIndex
	})

	// Now we ensure we have an expanded action instance for each action invocations.
	orderedActionData := make([]*actions.ActionData, len(orderedActionInvocations))
	for i, invocation := range orderedActionInvocations {
		ai, ok := ctx.Actions().GetActionInstance(invocation.Addr)
		if !ok {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Action instance not found",
				"Could not find action instance for address "+invocation.Addr.String(),
			))
			return finishedActionInvocations, diags
		}

		orderedActionData[i] = ai
	}

	// Now we have everything in place to execute the actions in the correct order.
	// TODO: Handle verifying the condition here, if we have any.

	// We run every action sequentially, as the order of execution is important. We also abort if
	// an action fails, as we don't want to continue executing actions or nodes that depend on it.

	for i, actionData := range orderedActionData {
		ai := orderedActionInvocations[i]
		provider, _, err := getProvider(ctx, actionData.ProviderAddr)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				fmt.Sprintf("Failed to get provider for %s", ai.Addr),
				fmt.Sprintf("Failed to get provider: %s", err),
			))
			return finishedActionInvocations, diags
		}

		// We don't want to send the marks, but all marks are okay in the context of an action invocation.
		unmarkedConfigValue, _ := actionData.ConfigValue.UnmarkDeep()

		hookIdentity := HookActionIdentity{
			Addr:                    ai.Addr,
			TriggeringResourceAddr:  ai.TriggeringResourceAddr,
			ActionTriggerBlockIndex: ai.ActionTriggerBlockIndex,
			ActionsListIndex:        ai.ActionsListIndex,
		}

		ctx.Hook(func(h Hook) (HookAction, error) {
			return h.StartAction(hookIdentity)
		})
		resp := provider.InvokeAction(providers.InvokeActionRequest{
			ActionType:        orderedActionInvocations[i].Addr.Action.Action.Type,
			PlannedActionData: unmarkedConfigValue,
		})

		diags = diags.Append(resp.Diagnostics)
		if resp.Diagnostics.HasErrors() {
			return finishedActionInvocations, diags
		}

		for event := range resp.Events {
			switch ev := event.(type) {
			case providers.InvokeActionEvent_Progress:
				ctx.Hook(func(h Hook) (HookAction, error) {
					return h.ProgressAction(hookIdentity, ev.Message)
				})
			case providers.InvokeActionEvent_Completed:
				diags = diags.Append(ev.Diagnostics)
				ctx.Hook(func(h Hook) (HookAction, error) {
					return h.CompleteAction(hookIdentity, ev.Diagnostics.Err())
				})
				if ev.Diagnostics.HasErrors() {
					// TODO: We would want to add some warning / error telling the user how to recover
					// from this, or maybe attach this info to the diagnostics sent by the provider.
					// For now we just return the diagnostics.

					return finishedActionInvocations, diags
				} else {
					finishedActionInvocations = append(finishedActionInvocations, ai)
				}
			default:
				panic(fmt.Sprintf("unexpected action event type %T", ev))
			}
		}
	}

	return finishedActionInvocations, diags
}

func (n *nodeActionApply) ModulePath() addrs.Module {
	return n.TriggeringResourceaddrs.Module.Module()
}

func (n *nodeActionApply) References() []*addrs.Reference {
	var refs []*addrs.Reference

	// We reference each action instance that we are going to execute.
	for _, invocation := range n.ActionInvocations {
		refs = append(refs, &addrs.Reference{
			Subject: invocation.Addr.Action,
		})
	}

	return refs
}

func (n *nodeActionApply) ActionProviders() []addrs.AbsProviderConfig {
	ret := []addrs.AbsProviderConfig{}
	for _, invocation := range n.ActionInvocations {
		ret = append(ret, invocation.ProviderAddr)
	}
	return ret
}

func betterDiags(triggeringResourceAddrs addrs.AbsResourceInstance, finishedActionInvocations, allActionInvocations []*plans.ActionInvocationInstance, diags tfdiags.Diagnostics) tfdiags.Diagnostics {
	// If everything went well, we can return the diagnostics as is.
	if !diags.HasErrors() {
		return diags
	}

	// Something went wrong, the user might need to take action so that
	// - the actions that failed or that were not executed can be retried
	// - the user can undo side-effects of actions that were executed successfully before the
	//   failure and will be re-run in the next apply.

	if areBeforeActionInvocations(allActionInvocations) {
		// Before actions need to let the user know that they will be re-run in the next apply
		alreadyRunActions := []string{}
		for _, ai := range finishedActionInvocations {
			alreadyRunActions = append(alreadyRunActions, fmt.Sprintf("- %s", ai.Addr))
		}

		return tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("Failed to apply actions before %s", triggeringResourceAddrs),
			Detail: fmt.Sprintf(
				`An error occured while invoking actions: %s
				
The following actions were successfully invoked:
%s

As the resource did not change, these actions will be re-invoked in the next apply.`,
				diags.ErrWithWarnings(),
				strings.Join(alreadyRunActions, "\n"),
			),
			// TODO: Add subject (source addrs range needs to be in action invocation?)
		})
	} else {
		missingActionInvocations := allActionInvocations[len(finishedActionInvocations):]
		missingActions := []string{}
		for _, ai := range missingActionInvocations {
			missingActions = append(missingActions, fmt.Sprintf("- %s", ai.Addr))
		}

		return tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("Failed to apply actions after %s", triggeringResourceAddrs),
			Detail: fmt.Sprintf(
				`An error occured while invoking actions: %s
				
The following actions were not yet invoked:
%s

These actions will not be triggered in the next apply, please run "terraform invoke" to invoke them.`,
				diags.ErrWithWarnings(),
				strings.Join(missingActions, "\n"),
			),
			// TODO: Add subject (source addrs range needs to be in action invocation?)
		})
	}

}

// areBeforeActionInvocations checks if all action invocations are for before actions.
// It panics if the action invocations are empty or if they have different trigger events.
func areBeforeActionInvocations(actionInvocations []*plans.ActionInvocationInstance) bool {
	if len(actionInvocations) == 0 {
		panic("areBeforeActionInvocations called with empty actionInvocations")
	}
	firstEvent := actionInvocations[0].TriggerEvent
	for _, ai := range actionInvocations {
		if ai.TriggerEvent != firstEvent {
			panic(fmt.Sprintf("areBeforeActionInvocations called with action invocations with different trigger events: %s != %s", firstEvent, ai.TriggerEvent))
		}
	}

	switch firstEvent {
	case configs.BeforeCreate, configs.BeforeUpdate, configs.BeforeDestroy:
		return true
	default:
		return false
	}
}
