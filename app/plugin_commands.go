// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-server/v6/app/request"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mattermost/mattermost-server/v6/shared/mlog"
)

type PluginCommand struct {
	Command  *model.Command
	PluginId string
}

func (a *App) RegisterPluginCommand(pluginID string, command *model.Command) error {
	return a.Srv().pluginService.registerPluginCommand(pluginID, command)
}

func (s *PluginService) registerPluginCommand(pluginID string, command *model.Command) error {
	if command.Trigger == "" {
		return errors.New("invalid command")
	}
	if command.AutocompleteData != nil {
		if err := command.AutocompleteData.IsValid(); err != nil {
			return errors.Wrap(err, "invalid autocomplete data in command")
		}
	}

	if command.AutocompleteData == nil {
		command.AutocompleteData = model.NewAutocompleteData(command.Trigger, command.AutoCompleteHint, command.AutoCompleteDesc)
	} else {
		baseURL, err := url.Parse("/plugins/" + pluginID)
		if err != nil {
			return errors.Wrapf(err, "Can't parse url %s", "/plugins/"+pluginID)
		}
		err = command.AutocompleteData.UpdateRelativeURLsForPluginCommands(baseURL)
		if err != nil {
			return errors.Wrap(err, "Can't update relative urls for plugin commands")
		}
	}

	command = &model.Command{
		Trigger:              strings.ToLower(command.Trigger),
		TeamId:               command.TeamId,
		AutoComplete:         command.AutoComplete,
		AutoCompleteDesc:     command.AutoCompleteDesc,
		AutoCompleteHint:     command.AutoCompleteHint,
		DisplayName:          command.DisplayName,
		AutocompleteData:     command.AutocompleteData,
		AutocompleteIconData: command.AutocompleteIconData,
	}

	s.pluginCommandsLock.Lock()
	defer s.pluginCommandsLock.Unlock()

	for _, pc := range s.pluginCommands {
		if pc.Command.Trigger == command.Trigger && pc.Command.TeamId == command.TeamId {
			if pc.PluginId == pluginID {
				pc.Command = command
				return nil
			}
		}
	}

	s.pluginCommands = append(s.pluginCommands, &PluginCommand{
		Command:  command,
		PluginId: pluginID,
	})
	return nil
}

func (a *App) UnregisterPluginCommand(pluginID, teamID, trigger string) {
	a.Srv().pluginService.unregisterPluginCommand(pluginID, teamID, trigger)
}

func (s *PluginService) unregisterPluginCommand(pluginID, teamID, trigger string) {
	trigger = strings.ToLower(trigger)

	s.pluginCommandsLock.Lock()
	defer s.pluginCommandsLock.Unlock()

	var remaining []*PluginCommand
	for _, pc := range s.pluginCommands {
		if pc.Command.TeamId != teamID || pc.Command.Trigger != trigger {
			remaining = append(remaining, pc)
		}
	}
	s.pluginCommands = remaining
}

func (s *PluginService) unregisterPluginCommands(pluginID string) {
	s.pluginCommandsLock.Lock()
	defer s.pluginCommandsLock.Unlock()

	var remaining []*PluginCommand
	for _, pc := range s.pluginCommands {
		if pc.PluginId != pluginID {
			remaining = append(remaining, pc)
		}
	}
	s.pluginCommands = remaining
}

func (a *App) PluginCommandsForTeam(teamID string) []*model.Command {
	return a.Srv().pluginService.PluginCommandsForTeam(teamID)
}

func (s *PluginService) PluginCommandsForTeam(teamID string) []*model.Command {
	s.pluginCommandsLock.RLock()
	defer s.pluginCommandsLock.RUnlock()

	var commands []*model.Command
	for _, pc := range s.pluginCommands {
		if pc.Command.TeamId == "" || pc.Command.TeamId == teamID {
			commands = append(commands, pc.Command)
		}
	}
	return commands
}

func (s *PluginService) getPluginCommandFromArgs(args *model.CommandArgs) *PluginCommand {
	parts := strings.Split(args.Command, " ")
	trigger := parts[0][1:]
	trigger = strings.ToLower(trigger)

	var matched *PluginCommand
	s.pluginCommandsLock.RLock()
	for _, pc := range s.pluginCommands {
		if (pc.Command.TeamId == "" || pc.Command.TeamId == args.TeamId) && pc.Command.Trigger == trigger {
			matched = pc
			break
		}
	}
	s.pluginCommandsLock.RUnlock()

	return matched
}

// tryExecutePluginCommand attempts to run a command provided by a plugin based on the given arguments. If no such
// command can be found, returns nil for all arguments.
func (a *App) tryExecutePluginCommand(c request.CTX, args *model.CommandArgs) (*model.Command, *model.CommandResponse, *model.AppError) {
	parts := strings.Split(args.Command, " ")
	trigger := parts[0][1:]
	trigger = strings.ToLower(trigger)

	matched := a.Srv().pluginService.getPluginCommandFromArgs(args)
	if matched == nil {
		return nil, nil, nil
	}

	pluginsEnvironment := a.GetPluginsEnvironment()
	if pluginsEnvironment == nil {
		return nil, nil, nil
	}

	// Checking if plugin is working or not
	if err := pluginsEnvironment.PerformHealthCheck(matched.PluginId); err != nil {
		return matched.Command, nil, model.NewAppError("ExecutePluginCommand", "model.plugin_command_error.error.app_error", map[string]any{"Command": trigger}, "err= Plugin has recently crashed: "+matched.PluginId, http.StatusInternalServerError)
	}

	pluginHooks, err := pluginsEnvironment.HooksForPlugin(matched.PluginId)
	if err != nil {
		return matched.Command, nil, model.NewAppError("ExecutePluginCommand", "model.plugin_command.error.app_error", nil, "err="+err.Error(), http.StatusInternalServerError)
	}

	for username, userID := range a.MentionsToTeamMembers(c, args.Command, args.TeamId) {
		args.AddUserMention(username, userID)
	}

	for channelName, channelID := range a.MentionsToPublicChannels(c, args.Command, args.TeamId) {
		args.AddChannelMention(channelName, channelID)
	}

	response, appErr := pluginHooks.ExecuteCommand(pluginContext(c), args)

	// Checking if plugin crashed after running the command
	if err := pluginsEnvironment.PerformHealthCheck(matched.PluginId); err != nil {
		errMessage := fmt.Sprintf("err= Plugin %s crashed due to /%s command", matched.PluginId, trigger)
		return matched.Command, nil, model.NewAppError("ExecutePluginCommand", "model.plugin_command_crash.error.app_error", map[string]any{"Command": trigger, "PluginId": matched.PluginId}, errMessage, http.StatusInternalServerError)
	}
	// This is a response from the plugin, which may set an incorrect status code;
	// e.g setting a status code of 0 will crash the server. So we always bucket everything under 500.
	if appErr != nil && (appErr.StatusCode < 100 || appErr.StatusCode > 999) {
		mlog.Warn("Invalid status code returned from plugin. Converting to internal server error.", mlog.String("plugin_id", matched.PluginId), mlog.Int("status_code", appErr.StatusCode))
		appErr.StatusCode = http.StatusInternalServerError
	}

	return matched.Command, response, appErr
}
