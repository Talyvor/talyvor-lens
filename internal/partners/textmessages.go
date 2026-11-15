package partners

import "context"

// ServiceTextMessages sends the texted code a person joins and signs in with (B61.22). Not in Services: it moves
// no money, so it has no rail.
const ServiceTextMessages Service = "text_messages"

// TextSender sends a sign-in code to a phone number.
type TextSender interface {
	// Name identifies the sender on every check it sends.
	Name() string
	// Sandbox is true for a sender that sends nothing: a number it confirms counts for nothing live.
	Sandbox() bool
	// SendCode texts code to phone (E.164).
	SendCode(ctx context.Context, phone, code string) error
}

// SandboxTextSender sends nothing: the caller hands the code back for the screen to show under the Sandbox label.
type SandboxTextSender struct{}

// Name is "sandbox".
func (SandboxTextSender) Name() string { return "sandbox" }

// Sandbox is true.
func (SandboxTextSender) Sandbox() bool { return true }

// SendCode sends nothing.
func (SandboxTextSender) SendCode(context.Context, string, string) error { return nil }

// TextMessages is the text sender: the configured one, else the Sandbox sender. A code moves no money, so no
// clearance is asked.
func (r *Registry) TextMessages() TextSender {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if adapter, ok := r.adapters[ServiceTextMessages]; ok {
		return adapter.(TextSender)
	}
	return SandboxTextSender{}
}
