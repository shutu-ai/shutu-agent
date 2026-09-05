package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shutu-ai/shutu-agent/internal/session"
)

// webCommand handles one resolved Web slash command. It keeps the existing
// Web-only acknowledgement event for rendering while also recording the
// canonical command/run + command/done lifecycle expected by dsh.
func (a *app) webCommand(ctx context.Context, line string) (returnErr error) {
	log := a.webLog(ctx)
	if log == nil {
		return errors.New("no active session")
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return errors.New("empty command")
	}
	name := fields[0]
	args := fields[1:]
	if !isWebCommandName(strings.TrimPrefix(name, "/")) {
		// The Web message adapter only calls this function for a leading slash
		// command. An admission miss must stay out of model history: the DSH
		// command executor returns no execution for unknown names, and the
		// adapter reports the miss to the caller without manufacturing a
		// user/assistant exchange.
		if strings.HasPrefix(name, "/") {
			return fmt.Errorf("unknown command %q (try /help)", name)
		}
		return fmt.Errorf("invalid command %q", name)
	}

	commandStart := log.NextSeq()
	commandID, err := appendCommandRun(log, strings.TrimPrefix(name, "/"), commandArgs(line, name))
	if err != nil {
		return err
	}
	commandKind := "success"
	commandText := ""
	defer func() {
		if returnErr != nil {
			commandKind = "error"
			commandText = returnErr.Error()
		}
		var source []uint64
		if commandKind == "success" {
			if seq := latestWebCommandResultSeq(log, commandStart); seq > 0 {
				source = []uint64{seq}
			}
		}
		if appendErr := appendCommandDone(log, commandID, commandKind, commandText, source...); returnErr == nil && appendErr != nil {
			returnErr = appendErr
		}
	}()

	if name == "/feedback" {
		result, feedbackErr := a.webFeedback(ctx, strings.TrimSpace(line[len(name):]))
		if feedbackErr != nil {
			commandKind = "error"
			commandText = feedbackErr.Error()
			result = "ERROR: " + feedbackErr.Error()
		} else {
			commandText = result
		}
		_, returnErr = log.Append(session.EventWebCommandResult, session.NewWebCommandResult(result, "feedback"))
		return returnErr
	}
	if name == "/plan" {
		var submit bool
		submit, returnErr = a.webPlanCommand(ctx, strings.TrimSpace(line[len(name):]))
		if returnErr == nil && submit && a.agentRegistry != nil {
			returnErr = a.runTurnFor(ctx, a.runtimeSessionID(ctx), strings.TrimSpace(line[len(name):]), false)
		}
		return returnErr
	}
	if name == "/export" {
		if len(args) > 0 {
			commandKind = "error"
			commandText = "The Web /export command does not accept a path."
			returnErr = a.appendWebCommandResultOn(log, "The Web /export command does not accept a path.")
			return returnErr
		}
		commandText = "Session log download requested."
		returnErr = a.appendWebCommandResultOn(log, commandText, "export")
		return returnErr
	}
	result, handlerErr := a.execWebCommand(ctx, name, args)
	if handlerErr != nil {
		commandKind = "error"
		commandText = handlerErr.Error()
		result = "ERROR: " + handlerErr.Error()
	} else {
		commandText = result
	}
	// Command output is a UI result, not a model turn. The reference command
	// service keeps it out of user/assistant history while still publishing a
	// durable, replayable projection for adapters that do not receive the
	// immediate execution response.
	_, returnErr = log.Append(session.EventWebCommandResult, session.NewWebCommandResult(result, strings.TrimPrefix(name, "/")))
	return returnErr
}
