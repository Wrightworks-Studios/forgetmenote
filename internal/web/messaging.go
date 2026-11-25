package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/taskqueue"
	"forgetmenote/internal/workspace"
)

func (s *Server) messageClient(id string) (messaging.Client, messaging.Transport, bool) {
	settings := s.catalog.ChatClients()
	var c messaging.Client
	var ok bool
	if settings.Revision > 0 {
		c, ok = settings.Clients[id]
	} else if id == "slack" && s.slack != nil {
		c, ok = s.slack.policy(), true
	}
	transport := s.messageClients[id]
	return c, transport, ok && !c.Disabled && transport != nil
}
func messageAddress(d messaging.Address) messaging.Address {
	if d.Client == "" {
		d.Client = "slack"
	}
	if d.User == "" {
		d.User = d.SlackUser
	}
	return d
}
func (s *Server) messageBinding(d messaging.Address) (messaging.Client, messaging.Binding, messaging.Transport, error) {
	d = messageAddress(d)
	c, t, ok := s.messageClient(d.Client)
	c = s.personalMessagePolicy(c, d)
	if !ok || d.Workspace != "" && d.Workspace != c.Workspace || d.Policy != "" && d.Policy != automation.Hash(c) {
		return c, messaging.Binding{}, nil, workspace.ErrForbidden
	}
	b, actor, member, ok := c.Resolve(d)
	if !ok || actor == "" || b.Shared != d.Shared {
		return c, b, t, workspace.ErrForbidden
	}
	if member {
		if _, ok := t.(messaging.MemberVerifier); !ok || !s.catalog.IsService(actor) {
			return c, b, t, workspace.ErrForbidden
		}
		c.Users = maps.Clone(c.Users)
		if c.Users == nil {
			c.Users = map[string]string{}
		}
		c.Users[d.User] = actor
	} else if !s.catalog.Interactive(actor) {
		return c, b, t, workspace.ErrForbidden
	}
	if b.Shared && (!b.AudienceApproved || !slices.Contains(b.Actors, c.Users[d.User])) {
		return c, b, t, workspace.ErrForbidden
	}
	actors := []string{c.Users[d.User]}
	if b.Shared {
		actors = b.Actors
	}
	for _, actor := range actors {
		if !member && !s.catalog.Interactive(actor) {
			return c, b, t, workspace.ErrForbidden
		}
		for _, project := range append([]string{b.Project}, b.ReadProjects...) {
			if _, err := s.catalog.Access(actor, project, "read"); err != nil {
				return c, b, t, err
			}
			for _, p := range s.catalog.List(actor) {
				if p.ID == project && p.ProtectedInbox != "" {
					return c, b, t, errors.New("private Inboxes cannot be exposed through chat integrations")
				}
			}
		}
	}
	return c, b, t, nil
}
func (s *Server) enqueueMessage(ctx context.Context, e messaging.Event) error {
	d := messageAddress(e.Address)
	c, _, ok := s.messageClient(d.Client)
	c = s.personalMessagePolicy(c, d)
	if !ok || c.Workspace != d.Workspace {
		return workspace.ErrForbidden
	}
	b, actor, _, ok := c.Resolve(d)
	if !ok {
		return workspace.ErrForbidden
	}
	d.Shared = b.Shared
	if _, _, _, err := s.messageBinding(d); err != nil {
		return err
	}
	if e.ID == "" || len(e.ID) > 128 || e.Text == "" && len(d.Files) == 0 || !messaging.ValidID(d.User) || !messaging.ValidID(d.Channel) || len(d.Thread) > 128 || len(d.Message) > 128 {
		return errors.New("invalid chat event")
	}
	if len(d.Files) > 0 && !b.Attachments {
		return errors.New("file processing is disabled for this destination")
	}
	for _, f := range d.Files {
		if !messaging.ValidID(f.ID) {
			return errors.New("invalid chat file reference")
		}
	}
	d.Policy = automation.Hash(c)
	if e.Text == "" {
		e.Text = "Use the attached files as context for this conversation."
	}
	// A transport may deliver both a mention and a message event for the same
	// message. Dedupe that message as well as retries of an individual event.
	key := e.ID
	if d.Message != "" {
		key = automation.Hash([]string{d.Channel, d.Message})
	}
	spec := automation.Spec{ID: "chat-" + automation.Hash([]string{d.Client, d.Workspace, key})[:32], Kind: "assistant", Actor: actor, Project: b.Project, ReadProjects: b.ReadProjects, Connections: b.Connections, AIConnection: b.AIConnection, Model: b.Model, ExternalProcessing: b.ExternalProcessing, Instruction: e.Text, Delivery: &d}
	if err := s.automationPolicy(spec); err != nil {
		return err
	}
	if d.Client == "slack" && messaging.StopRequested(e.Text) {
		if err := s.verifyMessageMember(ctx, d); err != nil {
			return err
		}
	}
	_, err := s.automations.Queue.Enqueue(spec, key)
	return err
}

// Re-resolve policy before provider calls, tool calls and delivery. A queued event
// never grants continued access after configuration or notebook permissions change.
func (s *Server) checkMessageSpec(spec automation.Spec) error {
	if spec.Delivery == nil {
		return workspace.ErrForbidden
	}
	d := messageAddress(*spec.Delivery)
	c, b, _, err := s.messageBinding(d)
	if err != nil {
		return err
	}
	if c.Users[d.User] != spec.Actor || b.Project != spec.Project || b.AIConnection != spec.AIConnection || b.Model != spec.Model || !b.ExternalProcessing || !slices.Equal(b.Connections, spec.Connections) || !slices.Equal(b.ReadProjects, spec.ReadProjects) {
		return workspace.ErrForbidden
	}
	return nil
}

func (s *Server) verifyMessageMember(ctx context.Context, d messaging.Address) error {
	d = messageAddress(d)
	c, transport, ok := s.messageClient(d.Client)
	c = s.personalMessagePolicy(c, d)
	if !ok {
		return workspace.ErrForbidden
	}
	_, _, member, bound := c.Resolve(d)
	if !bound {
		return workspace.ErrForbidden
	}
	if !member {
		if s.personalConnections != nil && s.personalConnections.LinkedOwner(d.Client, c.Workspace, d.User) != "" {
			if verifier, ok := transport.(messaging.MemberVerifier); ok {
				return verifier.VerifyMember(ctx, d)
			}
		}
		return nil
	}
	verifier, ok := transport.(messaging.MemberVerifier)
	if !ok {
		return workspace.ErrForbidden
	}
	return verifier.VerifyMember(ctx, d)
}

func messageConversation(c messaging.Client, b messaging.Binding, d messaging.Address, actor string) (string, string) {
	if b.Shared {
		var audience any = c.Users
		if c.Members != nil && actor == c.Members.Actor {
			// The temporary requester-to-service mapping must not split a public
			// workspace thread into one transcript per requesting member.
			audience = c.Members
		}
		return "chat-client:" + automation.Hash([]string{d.Client, c.Workspace, d.Channel}), automation.Hash([]any{d.Client, c.Workspace, d.Channel, d.Thread, b, audience})
	}
	return actor, automation.Hash([]any{d.Client, c.Workspace, d.Channel, d.Thread, d.User, b})
}

var errChatGenerationUnknown = errors.New("chat generation outcome is unknown; automatic repeat suppressed")

// messageRoute is where a message run answers: the client policy, the
// destination's binding, its address and the transport that delivers there.
type messageRoute struct {
	client    messaging.Client
	binding   messaging.Binding
	address   messaging.Address
	transport messaging.Transport
}

func (s *Server) runMessageAutomation(ctx context.Context, r *automation.Run, save func() error) (runErr error) {
	// The answer checks its member before each provider call and its delivery;
	// one check serves them for a short while.
	ctx = messaging.RememberMembers(ctx)
	if err := s.checkMessageSpec(r.Spec); err != nil {
		return err
	}
	d := messageAddress(*r.Spec.Delivery)
	c, b, transport, err := s.messageBinding(d)
	if err != nil {
		return err
	}
	if err := s.verifyMessageMember(ctx, d); err != nil {
		return err
	}
	if done, err := messageDelivered(r); done || err != nil {
		return err
	}
	route := messageRoute{client: c, binding: b, address: d, transport: transport}
	if reply, ok, err := s.messageCommandReply(ctx, r, route); ok || err != nil {
		if err != nil {
			return err
		}
		return s.sendMessage(ctx, transport, d, reply, r, save)
	}
	stopProgress := s.messageProgress(ctx, transport, d)
	defer stopProgress()
	defer func() {
		if runErr != nil && r.DeliveryState == "" && r.Answer == "" {
			runErr = s.messageFailure(ctx, transport, d, r, save, runErr)
		}
	}()
	if r.Answer == "" {
		if err = s.answerMessage(ctx, r, save, route); err != nil {
			return err
		}
	}
	if err = s.checkAutomationEvidence(ctx, r, true); err != nil {
		return err
	}
	if err = s.messageArtifactsAllowed(ctx, r); err != nil {
		return err
	}
	text := r.Answer
	for _, a := range r.Artifacts {
		if a.Caption != "" {
			text += "\n\n" + a.Caption
		}
	}
	// Memory capture is best effort, as in notebook chat; it never withholds the answer.
	if d.Direct && s.catalog.Interactive(r.Spec.Actor) {
		if memoryErr := s.recordPersonalInteraction(ctx, r.Spec.Actor, personalInteraction{ID: "message-" + r.ID, UserText: r.Spec.Instruction, Response: r.Answer, Source: d.Client + ":" + d.Workspace + "/" + d.Channel + "/" + d.Message}); memoryErr != nil {
			slog.Warn("personal memory capture failed", "error", memoryErr)
		}
	}
	return s.sendMessage(ctx, transport, d, messaging.Reply{Text: text, Artifacts: r.Artifacts, Confirmations: writeConfirmations(r.WriteProposals)}, r, save)
}

// messageDelivered reports whether a run's reply was already sent. A send or
// failure notice with an unknown outcome is never repeated automatically.
func messageDelivered(r *automation.Run) (bool, error) {
	switch {
	case r.DeliveryState == "sent":
		return true, nil
	case r.DeliveryState == "sending":
		return true, errors.New("chat delivery outcome is unknown; automatic resend suppressed")
	case strings.HasPrefix(r.DeliveryState, "failure-"):
		return true, errors.New("the previous request failed; automatic repeat suppressed")
	}
	return false, nil
}

// messageCommandReply answers linking and task commands, never the model,
// so a task is exactly what its requester typed. ok reports a command.
func (s *Server) messageCommandReply(ctx context.Context, r *automation.Run, route messageRoute) (messaging.Reply, bool, error) {
	d := route.address
	if command := messageLinkCommand(r.Spec.Instruction); command != "" && d.Direct && !d.Shared && r.Answer == "" {
		text, err := s.messageLinkReply(r, route.client, d, command)
		return messaging.Reply{Text: text}, true, err
	}
	if taskqueue.Command(r.Spec.Instruction) && r.Answer == "" && !r.GenerationStarted {
		return s.tasks.CommandReply(ctx, r, d)
	}
	return messaging.Reply{}, false, nil
}

// answerMessage answers in the destination's bound conversation, once: it
// recovers an answer the conversation already records for this run, handles
// a confirmation code itself, prepares the thread context once, and records
// the new turn with the results and artifacts it relies on.
func (s *Server) answerMessage(ctx context.Context, r *automation.Run, save func() error, route messageRoute) error {
	owner, binding := messageConversation(route.client, route.binding, route.address, r.Spec.Actor)
	_, connection, _, err := s.automationProvider(r.Spec)
	if err != nil {
		return err
	}
	model := r.Spec.Model
	if model == "" {
		model = connection.DefaultModel
	}
	options := chat.Options{Mode: "ask", Connection: connection.ID, Model: model, Corpus: chat.RunScopeFrom(ctx).Run.Options.Corpus}
	conversation, err := s.chat.Store.OpenBound(owner, r.Spec.Project, binding, options)
	if err != nil {
		return err
	}
	history := s.messageHistory(ctx, r, conversation)
	// A confirmation code is handled here, never by the model.
	confirming, cancelling := "", false
	if r.Answer == "" && !r.GenerationStarted {
		confirming, cancelling = writeConfirmation(r.Spec.Instruction)
	}
	if confirming != "" {
		if r.Answer, err = s.confirmDataWrite(ctx, r, owner, conversation, route.address, confirming, cancelling); err != nil {
			return err
		}
	}
	if !r.ChatPrepared && confirming == "" {
		if err = s.prepareMessageContext(ctx, r, save, route); err != nil {
			return err
		}
	}
	evidence := chat.Run{}
	addChatCitations(&evidence, r.Citations)
	r.Citations = evidence.Citations
	s.recoverMessageResults(ctx, r, conversation)
	if r.Answer == "" {
		// The answer depends on the notebook guidance it was given.
		r.Answer, err = s.answerOnce(ctx, r, save, errChatGenerationUnknown, func() (err error) {
			_, r.ChatGuidanceHash, err = s.assistantInstructions(ctx, r.Spec)
			return err
		}, r.Spec.Instruction, true, append(history, r.ChatContext...)...)
		if err != nil {
			return err
		}
		r.Answer += writeProposalText(r.WriteProposals)
	}
	if err = s.recordMessageTurn(r, owner, conversation.ID, options, route.address); err != nil {
		return err
	}
	return save()
}

// messageHistory is the conversation before this run, as model messages. It
// also recovers this run's answer and citations if the conversation already
// records them.
func (s *Server) messageHistory(ctx context.Context, r *automation.Run, conversation chat.Conversation) []chat.Message {
	var history []chat.Message
	for _, turn := range conversation.Turns {
		if turn.RunID == r.ID && turn.Role == "assistant" {
			r.Answer = turn.Content
			r.Citations = append(r.Citations, turn.Citations...)
		}
		if turn.RunID == r.ID || turn.Role != "user" && turn.Role != "assistant" {
			continue
		}
		if turn.Role == "assistant" && len(turn.Citations) > 0 {
			prior := chat.RunScopeFrom(ctx).Run
			prior.Citations = turn.Citations
			if err := s.validateChatSources(ctx, prior, true); err != nil {
				// Old answer dependencies must not pin a new question to
				// obsolete evidence. Current-run recovery remains fenced above.
				history = append(history, chat.Message{Role: "assistant", Content: "[Earlier answer omitted because its sources changed or are no longer available; retrieve current evidence.]"})
				continue
			}
		}
		history = append(history, chat.Message{Role: turn.Role, Content: turn.Content})
		for _, a := range turn.Attachments {
			if a.Kind == "chat-document" {
				history = append(history, chat.Message{Role: "user", Content: a.Content})
			}
		}
		r.Citations = append(r.Citations, turn.Citations...)
	}
	return history
}

// prepareMessageContext reads the thread once: messages by authors who may
// share it and the attached documents, both as untrusted context.
func (s *Server) prepareMessageContext(ctx context.Context, r *automation.Run, save func() error, route messageRoute) error {
	c, b, d := route.client, route.binding, route.address
	input, err := route.transport.Context(ctx, d, b)
	if err != nil {
		return err
	}
	verifiedAuthors := map[string]bool{}
	for _, m := range input.Messages {
		if m.ID == d.Message {
			continue
		}
		// Workspace-wide shared threads use one service identity, but
		// their context still belongs to verified human authors.
		if b.Shared && c.Members != nil && r.Spec.Actor == c.Members.Actor {
			verified, checked := verifiedAuthors[m.User]
			if !checked {
				author := d
				author.User = m.User
				err := s.verifyMessageMember(ctx, author)
				if err != nil && !errors.Is(err, workspace.ErrForbidden) {
					return err
				}
				verified = err == nil
				verifiedAuthors[m.User] = verified
			}
			if verified {
				r.ChatContext = append(r.ChatContext, chat.Message{Role: "user", Content: "Thread message from " + m.User + ":\n" + m.Text})
			}
			continue
		}
		actor := c.Users[m.User]
		if actor == "" || (!s.catalog.Interactive(actor) && !(s.catalog.IsService(actor) && m.User == d.User)) || (!b.Shared && actor != r.Spec.Actor) || b.Shared && !slices.Contains(b.Actors, actor) {
			continue
		}
		r.ChatContext = append(r.ChatContext, chat.Message{Role: "user", Content: "Thread message from " + m.User + ":\n" + m.Text})
	}
	for _, doc := range input.Documents {
		content := fmt.Sprintf("Attached document %s\nSource: %s\nContent hash: %s\nExtraction: %s\nUntrusted document content:\n%s", doc.Name, doc.URL, doc.Hash, doc.Warning, doc.Text)
		r.ChatDocuments = append(r.ChatDocuments, chat.Attachment{Kind: "chat-document", Path: doc.URL, Content: content, Hash: doc.Hash})
		r.ChatContext = append(r.ChatContext, chat.Message{Role: "user", Content: content})
	}
	r.ChatPrepared = true
	return save()
}

// recoverMessageResults reuses only retained source results, never
// model-provided rows: on a follow-up, and with a recovered answer, also the
// artifacts this run recorded.
func (s *Server) recoverMessageResults(ctx context.Context, r *automation.Run, conversation chat.Conversation) {
	if r.Answer != "" {
		for _, run := range conversation.Runs {
			if run.ID != r.ID {
				continue
			}
			for _, op := range run.Operations {
				var a messaging.Artifact
				if op.Name == "chat_artifact" && json.Unmarshal(op.Result, &a) == nil {
					r.Artifacts = append(r.Artifacts, a)
				}
			}
		}
	}
	if len(r.ChatResults) > 0 && r.Answer == "" {
		return
	}
	for _, run := range conversation.Runs {
		for _, op := range run.Operations {
			var result connectors.Result
			if op.Name == "chat_result" && json.Unmarshal(op.Result, &result) == nil && s.dataAllowed(ctx, result.Receipt.Connection, result.Receipt.Operation) {
				retainMessageResult(r, result)
			}
		}
	}
}

// recordMessageTurn adds this run's question and answer to the conversation
// once, with one bounded cache of source results for the whole conversation.
func (s *Server) recordMessageTurn(r *automation.Run, owner, conversation string, options chat.Options, d messaging.Address) error {
	return s.chat.Store.Update(owner, r.Spec.Project, conversation, func(v *chat.Conversation) error {
		for _, turn := range v.Turns {
			if turn.RunID == r.ID && turn.Role == "assistant" {
				return nil
			}
		}
		v.Title = "Chat conversation"
		v.Options = options
		if d.Client == "slack" {
			v.Title = "Slack conversation"
		}
		v.Turns = append(v.Turns, chat.Turn{ID: chat.NewID(), RunID: r.ID, Role: "user", Content: r.Spec.Instruction, Attachments: r.ChatDocuments}, chat.Turn{ID: chat.NewID(), RunID: r.ID, Role: "assistant", Content: r.Answer, Citations: r.Citations})
		// Keep one bounded result cache for the conversation. A follow-up export
		// must not duplicate megabytes of old query rows in every run.
		for i := range v.Runs {
			v.Runs[i].Operations = slices.DeleteFunc(v.Runs[i].Operations, func(op chat.Operation) bool { return op.Name == "chat_result" })
		}
		run := chat.Run{ID: r.ID, SubmissionID: r.ID, State: "completed", CreatedAt: chat.Now(), UpdatedAt: chat.Now(), Options: v.Options, Citations: r.Citations, Usage: chat.Accounting{InputTokens: int64(r.InputTokens), OutputTokens: int64(r.OutputTokens)}}
		for _, result := range r.ChatResults {
			raw, _ := json.Marshal(result)
			run.Operations = append(run.Operations, chat.Operation{Name: "chat_result", Result: raw})
		}
		for _, a := range r.Artifacts {
			raw, _ := json.Marshal(a)
			run.Operations = append(run.Operations, chat.Operation{Name: "chat_artifact", Result: raw})
		}
		run.Operations = append(append(run.Operations, r.WriteProposals...), r.MemoryOperations...)
		v.Runs = append(v.Runs, run)
		return nil
	})
}

// messageArtifactsAllowed checks that every result-bound artifact still has
// a retained result the run may disclose.
func (s *Server) messageArtifactsAllowed(ctx context.Context, r *automation.Run) error {
	for _, a := range r.Artifacts {
		if a.Receipt == "" {
			continue
		}
		if !slices.ContainsFunc(r.ChatResults, func(result connectors.Result) bool {
			return result.Receipt.ID == a.Receipt && s.dataAllowed(ctx, result.Receipt.Connection, result.Receipt.Operation)
		}) {
			return workspace.ErrForbidden
		}
	}
	return nil
}

func (s *Server) sendMessage(ctx context.Context, t messaging.Transport, d messaging.Address, reply messaging.Reply, r *automation.Run, save func() error) error {
	if r.DeliveryState == "sent" {
		return nil
	}
	if r.DeliveryState == "sending" {
		return errors.New("chat delivery outcome is unknown; automatic resend suppressed")
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.Artifacts) == 0 {
		return errors.New("empty assistant reply")
	}
	// Browser-relative citations need the instance origin in external clients.
	// Rewrite only server-retained citation targets, leaving other links intact.
	for _, citation := range r.Citations {
		if strings.HasPrefix(citation.URL, "/") && !strings.HasPrefix(citation.URL, "//") {
			reply.Text = strings.ReplaceAll(reply.Text, "]("+citation.URL+")", "]("+s.origin+citation.URL+")")
		}
	}
	r.DeliveryState = "sending"
	if err := save(); err != nil {
		return err
	}
	send, cancel := context.WithTimeout(ctx, replySendTimeout)
	err := t.Send(send, d, reply)
	cancel()
	switch sendOutcome(err) {
	case "accepted":
		r.DeliveryState = "sent"
		return save()
	case "rejected":
		// Nothing reached the conversation, so running the request again may
		// send it.
		r.DeliveryState = ""
		return errors.Join(err, save())
	}
	return err
}

// Every send is bounded: a direct message by dmSendTimeout, and a reply in a
// conversation, with its files, by replySendTimeout. A message Slack refuses
// is tried at most sendAttempts times.
const (
	dmSendTimeout    = 10 * time.Second
	replySendTimeout = 2 * time.Minute
	sendAttempts     = 5
)

// sendOutcome names a send's outcome: accepted; rejected when nothing reached
// the conversation, so it may be sent again; or unknown, when it may have
// arrived and must not be.
func sendOutcome(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, messaging.ErrNotPosted):
		return "rejected"
	}
	return "unknown"
}

// sendOnce sends reply to d at most once for the message effect names, within
// timeout, and keeps its outcome with receipt. sent reports whether this call
// delivered it; it is false with no error when an earlier call did.
//
// A send that reached nobody is recorded rejected and returned, and calling
// again tries again, as the next attempt of the same action. Any other failure
// is recorded unknown, which holds back every later message in the effect's
// scope until an administrator resolves it. An error recording the effect is
// returned and nothing is sent. Once begun, a send is not cancelled with ctx,
// since that would leave its outcome unknown.
func (s *Server) sendOnce(ctx context.Context, effect automation.Effect, t messaging.Transport, d messaging.Address, reply messaging.Reply, timeout time.Duration, receipt map[string]any) (sent bool, err error) {
	if effect.InputHash == "" {
		effect.InputHash = automation.Hash(reply.Text)
	}
	effect.Connection, effect.Operation = d.Client, "notify"
	action := effect.Action
	var begun automation.Effect
	for attempt := 1; begun.ID == ""; attempt++ {
		if attempt > 1 {
			effect.Action = action + "#" + strconv.Itoa(attempt)
		}
		e, created, err := s.automations.Queue.BeginEffect(effect)
		switch {
		case err == nil && created:
			begun = e
		case e.State == "accepted":
			return false, nil
		case err != nil && e.State == "rejected" && attempt < sendAttempts:
		case err != nil && e.State == "rejected":
			return false, fmt.Errorf("Slack refused this message %d times; it is not sent again", sendAttempts)
		default:
			return false, err
		}
	}
	send, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	err = t.Send(send, d, reply)
	cancel()
	outcome := sendOutcome(err)
	result := map[string]any{"delivered": outcome == "accepted"}
	maps.Copy(result, receipt)
	if _, finish := s.automations.Queue.FinishEffect(begun.ID, outcome, result); finish != nil {
		err = errors.Join(err, finish)
	}
	return outcome == "accepted", err
}
