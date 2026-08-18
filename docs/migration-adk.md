# Migration to Google ADK 2.0 & Vertex AI Gemini

## Executive Summary
This document records the architectural migration of `ask` from `charm.land/fantasy` to Google's official **ADK 2.0** (`google.golang.org/adk/v2`) and the `genai` Go SDK (`google.golang.org/genai`).

As part of this transition, `ask` has streamlined its provider surface to focus on **Vertex AI Gemini**, removing all legacy third-party provider implementations while maintaining exact 1:1 parity for sessions, token/cost accounting, reasoning effort levels, dynamic model querying, and tool execution.

---

## Rationale for ADK 2.0
1. **Native Gemini & Vertex AI Support**: ADK 2.0 is Google's official Go agent framework. It provides first-class support for Gemini 2.5 and 3.x models, thinking token budgets, and thought signatures.
2. **Robust Multi-Turn Orchestration**: ADK's `llmagent`, `session.Service`, and `runner.Runner` manage the multi-turn tool calling loop natively, eliminating manual message serialization bugs.
3. **DAGs and Sub-Agent Primitives**: ADK provides native sequential, parallel, and graph-based workflow engines.
4. **Clean Authentication**: Pass project, location, and credentials directly into `genai.ClientConfig` without mutating process-global environment variables.

---

## Removed Providers Catalog
The following legacy provider implementations were removed during this migration:

| Provider | Former Implementation File | Description | Notes for Re-introduction |
|---|---|---|---|
| **Anthropic** | `pkg/providers/anthropic.go` | Anthropic Claude Messages API integration with prompt caching | Can be re-introduced by implementing ADK's `model.LLM` interface (Messages API). |
| **DeepSeek** | `pkg/providers/deepseek.go` | OpenAI-compatible reasoning endpoint (`reasoning_content`) | Can be re-introduced as an ADK `model.LLM` wrapper. |
| **Google AI Studio** | `pkg/providers/googleai.go` | Direct Gemini API Key integration | Replaced by unified Vertex AI Gemini provider; API-key auth can be supported via `genai.ClientConfig{APIKey: ...}`. |
| **Kimi / Moonshot** | `pkg/providers/kimi.go` | Moonshot Kimi Chat Completions integration | Can be re-introduced as an ADK `model.LLM` wrapper. |
| **MiniMax** | `pkg/providers/minimax.go` | MiniMax Anthropic-compatible Messages integration | Can be re-introduced as an ADK `model.LLM` wrapper (see `//apps/zeus/internal/llm/m3/`). |
| **OpenAI** | `pkg/providers/openai.go` | OpenAI Responses API integration | Can be re-introduced by implementing ADK's `model.LLM` interface. |

---

## Vertex AI Provider Architecture
The active provider is **Vertex AI** (`providers.VertexSpec`):

- **Model Constructor**: `providers.VertexModel` initializes a `model.LLM` using `google.golang.org/adk/v2/model/gemini` with `genai.ClientConfig{Backend: genai.BackendVertexAI, Project: project, Location: location}`.
- **Dynamic Model Querying**: `providers.ListVertexModels` queries `client.Models.List` dynamically against Vertex AI, filtering out non-Gemini IDs and caching results to prevent hardcoded model lists.
- **Thinking / Effort Mapping**: `providers.VertexProviderOptions` maps ask effort levels (`low`, `medium`, `high`, `minimal`, `off`) to `genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: level}`.
- **Tool Calling**: Native tools in `pkg/tools/` are converted to ADK `tool.Tool` instances using `functiontool.New[map[string]any, any]` with auto-derived JSON schemas.
- **Thought Signatures**: `genai.Part.ThoughtSignature` is preserved on all function call and thought parts across the runner stream, message structs, and session storage.
