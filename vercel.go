package jev

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
)

// Vercel AI Gateway serves Jev at its own endpoint, in the AI SDK's
// evaluation-model dialect rather than TypeSafe's. [WithVercelAIGateway]
// selects it; everything above the wire (questions, answers, batches,
// errors, retries) is unchanged.
const (
	// DefaultVercelAIGatewayBaseURL is the gateway's AI SDK protocol root.
	// Evaluation is posted to <base>/evaluation-model.
	DefaultVercelAIGatewayBaseURL = "https://ai-gateway.vercel.sh/v4/ai"
	// DefaultVercelAIGatewayModel is Jev's id on the gateway.
	DefaultVercelAIGatewayModel = "typesafe-ai/jev"
	// EnvVercelAIGatewayAPIKey is the environment variable the gateway
	// credential is read from when [WithAPIKey] is not given. It is the
	// same variable the AI SDK reads.
	EnvVercelAIGatewayAPIKey = "AI_GATEWAY_API_KEY"

	vercelPathEvaluation = "/evaluation-model"

	// Headers the AI SDK gateway provider sends, as of @ai-sdk/gateway 4.0.
	vercelProtocolHeader         = "Ai-Gateway-Protocol-Version"
	vercelProtocolVersion        = "0.0.1"
	vercelAuthMethodHeader       = "Ai-Gateway-Auth-Method"
	vercelAuthMethod             = "api-key"
	vercelSpecVersionHeader      = "Ai-Evaluation-Model-Specification-Version"
	vercelSpecVersion            = "4"
	vercelModelHeader            = "Ai-Model-Id"
	vercelRequestIDHeader        = "X-Vercel-Id"
	vercelBooleanKind       Kind = "boolean"
)

// WithVercelAIGateway routes requests through Vercel AI Gateway instead of
// the TypeSafe API. The key comes from [WithAPIKey] or AI_GATEWAY_API_KEY,
// the base URL from [WithBaseURL] or [DefaultVercelAIGatewayBaseURL], and
// the model from [WithModel] or [DefaultVercelAIGatewayModel] ("typesafe-ai/jev";
// TYPESAFE_DEFAULT_MODEL is not consulted because gateway ids differ).
// [Client.Models] is not available through the gateway.
//
// On the wire, noul questions are sent as the gateway's "boolean" kind and
// its answers are mapped back, so [Noul], [Choice], and [Score] behave
// exactly as against TypeSafe. [Request.Extra] fields travel as top-level
// request fields, which is where the gateway's providerOptions go. Choice
// and score confidence comes from providerMetadata.typesafe.confidence.
func WithVercelAIGateway() Option {
	return func(c *config) { c.vercel = true }
}

// vercelWire speaks the AI SDK evaluation-model protocol of Vercel AI
// Gateway.
type vercelWire struct{}

func (vercelWire) evaluatePath() string    { return vercelPathEvaluation }
func (vercelWire) requestIDHeader() string { return vercelRequestIDHeader }
func (vercelWire) listsModels() bool       { return false }

// vercelRequest is the gateway's body: the model travels in a header.
type vercelRequest struct {
	State     any             `json:"state"`
	Questions map[string]Spec `json:"questions"`
	Extra     map[string]any  `json:",embed"`
}

func (vercelWire) encode(req *Request) ([]byte, error) {
	questions := make(map[string]Spec, len(req.Questions))
	for name, q := range req.Questions {
		if q.Type == KindNoul {
			q.Type = vercelBooleanKind
		}
		questions[name] = q
	}
	return json.Marshal(vercelRequest{State: req.State, Questions: questions, Extra: req.Extra}, json.Deterministic(true))
}

func (vercelWire) setHeaders(h http.Header, req *Request, _ int) {
	h.Set(vercelProtocolHeader, vercelProtocolVersion)
	h.Set(vercelAuthMethodHeader, vercelAuthMethod)
	h.Set(vercelSpecVersionHeader, vercelSpecVersion)
	if req != nil {
		h.Set(vercelModelHeader, req.Model)
	}
}

// vercelAnswer is one answer in the gateway's discriminated union.
type vercelAnswer struct {
	Type          Kind                      `json:"type"`
	Probability   *float64                  `json:"probability,omitzero"`
	Choice        *string                   `json:"choice,omitzero"`
	Score         *float64                  `json:"score,omitzero"`
	Probabilities map[string]float64        `json:"probabilities,omitzero"`
	Extra         map[string]jsontext.Value `json:",embed"`
}

type vercelWarning struct {
	Type    string `json:"type"`
	Feature string `json:"feature,omitzero"`
	Details string `json:"details,omitzero"`
	Setting string `json:"setting,omitzero"`
	Message string `json:"message,omitzero"`
}

type vercelResponse struct {
	Answers map[string]vercelAnswer `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"usage,omitzero"`
	Warnings         []vercelWarning                      `json:"warnings,omitzero"`
	ProviderMetadata map[string]map[string]jsontext.Value `json:"providerMetadata,omitzero"`
}

func (vercelWire) decode(req *Request, res result) (*Response, []string, error) {
	var in vercelResponse
	if err := json.Unmarshal(res.body, &in); err != nil {
		return nil, nil, newResponseError(res, jsonPath(err), err)
	}
	if in.Answers == nil {
		return nil, nil, newResponseError(res, "answers", errMissingField)
	}
	// TypeSafe's per-question confidence rides along in provider metadata.
	confidence := map[string]float64{}
	if raw, ok := in.ProviderMetadata["typesafe"]["confidence"]; ok {
		if err := json.Unmarshal(raw, &confidence); err != nil {
			return nil, nil, newResponseError(res, "providerMetadata.typesafe.confidence", err)
		}
	}
	out := &Response{Model: req.Model, Answers: make(map[string]RawAnswer, len(in.Answers))}
	for name, a := range in.Answers {
		raw := RawAnswer{Type: a.Type, Extra: a.Extra}
		switch a.Type {
		case vercelBooleanKind:
			raw.Type = KindNoul
			raw.Noul = a.Probability
		case KindChoice:
			raw.Choice = a.Choice
			raw.Probabilities = a.Probabilities
		case KindScore:
			raw.Score = a.Score
			raw.Probabilities = a.Probabilities
		default:
			// A kind this package predates: keep every field for Raw questions.
			if raw.Extra == nil {
				raw.Extra = map[string]jsontext.Value{}
			}
			for k, v := range map[string]any{"probability": a.Probability, "choice": a.Choice, "score": a.Score, "probabilities": a.Probabilities} {
				if b, err := json.Marshal(v); err == nil && string(b) != "null" {
					raw.Extra[k] = b
				}
			}
		}
		if c, ok := confidence[name]; ok {
			raw.Confidence = &c
		}
		out.Answers[name] = raw
	}
	if in.Usage != nil {
		out.Usage = Usage{InputTokens: in.Usage.InputTokens, OutputTokens: in.Usage.OutputTokens}
	}
	var warnings []string
	for _, w := range in.Warnings {
		warnings = append(warnings, w.String())
	}
	return out, warnings, nil
}

func (w vercelWarning) String() string {
	switch {
	case w.Message != "" && w.Setting != "":
		return fmt.Sprintf("%s: %s: %s", w.Type, w.Setting, w.Message)
	case w.Message != "":
		return fmt.Sprintf("%s: %s", w.Type, w.Message)
	case w.Details != "":
		return fmt.Sprintf("%s: %s: %s", w.Type, w.Feature, w.Details)
	}
	return fmt.Sprintf("%s: %s", w.Type, w.Feature)
}
