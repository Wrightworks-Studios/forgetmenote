package catalog

import (
	"maps"
	"slices"
)

// cloneCatalog isolates the complete permission transaction, including nested
// policy and audit values. Scalar values and immutable time.Time values copy.
func cloneCatalog(in catalogData) catalogData {
	out := in
	out.Users = maps.Clone(in.Users)
	out.Passwords = maps.Clone(in.Passwords)
	out.OIDC = slices.Clone(in.OIDC)
	out.MessageLinks = slices.Clone(in.MessageLinks)
	out.MessageDeclines = slices.Clone(in.MessageDeclines)
	out.TaskPeople = slices.Clone(in.TaskPeople)
	if in.TaskQueues != nil {
		value := *in.TaskQueues
		value.AwayStatuses = slices.Clone(value.AwayStatuses)
		value.HolidayTitles = slices.Clone(value.HolidayTitles)
		out.TaskQueues = &value
	}
	out.AccessEpoch = maps.Clone(in.AccessEpoch)
	out.ProjectAccessEpoch = maps.Clone(in.ProjectAccessEpoch)
	out.Transfers = maps.Clone(in.Transfers)
	out.InstanceLogo = slices.Clone(in.InstanceLogo)
	out.Accounts = maps.Clone(in.Accounts)
	for id, value := range out.Accounts {
		out.Accounts[id] = cloneAccount(value)
	}
	out.Assistants = maps.Clone(in.Assistants)
	for id, value := range out.Assistants {
		out.Assistants[id] = cloneAssistant(value)
	}
	out.Groups = maps.Clone(in.Groups)
	for id, value := range out.Groups {
		value.Members = slices.Clone(value.Members)
		out.Groups[id] = value
	}
	out.Invitations = maps.Clone(in.Invitations)
	for id, value := range out.Invitations {
		value.Groups = slices.Clone(value.Groups)
		value.Projects = maps.Clone(value.Projects)
		out.Invitations[id] = value
	}
	out.ConnectionGrants = maps.Clone(in.ConnectionGrants)
	for id, value := range out.ConnectionGrants {
		if value.Users != nil {
			value.Users = cloneOperations(value.Users)
		}
		if value.Groups != nil {
			value.Groups = cloneOperations(value.Groups)
		}
		out.ConnectionGrants[id] = value
	}
	out.Activity = slices.Clone(in.Activity)
	for i := range out.Activity {
		out.Activity[i].Details = maps.Clone(out.Activity[i].Details)
	}
	out.Projects = maps.Clone(in.Projects)
	for id, value := range out.Projects {
		value.Members = maps.Clone(value.Members)
		value.GroupRoles = maps.Clone(value.GroupRoles)
		value.EffectiveMembers = maps.Clone(value.EffectiveMembers)
		for member, access := range value.EffectiveMembers {
			access.Groups = slices.Clone(access.Groups)
			value.EffectiveMembers[member] = access
		}
		if value.Authority != nil {
			authority := *value.Authority
			value.Authority = &authority
		}
		if value.Capabilities != nil {
			capabilities := *value.Capabilities
			value.Capabilities = &capabilities
		}
		out.Projects[id] = value
	}
	if in.AI != nil {
		value := *in.AI
		out.AI = &value
	}
	if in.AssistantPolicy != nil {
		value := *in.AssistantPolicy
		out.AssistantPolicy = &value
	}
	if in.ChatClients != nil {
		value := cloneChatClients(*in.ChatClients)
		out.ChatClients = &value
	}
	return out
}
