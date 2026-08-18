# Provider Migration & Deprecation Log

## Overview
As part of the migration from `charm.land/fantasy` to Google ADK (Agent Development Kit) on Vertex AI, all non-Vertex AI providers have been deprecated and stripped from the active provider registry.

## Deprecated & Removed Providers
The following providers have been removed:
1. **Anthropic** (`anthropic`)
   - Replaced by native Gemini on Vertex AI.
   - Prompt caching and Claude-specific breakpoints deprecated.
2. **OpenAI** (`openai`)
   - Responses API and encrypted reasoning handling removed.
3. **DeepSeek** (`deepseek`)
   - Prefix cache handling and OpenAI-compatible client removed.
4. **Google AI Studio** (`googleai`)
   - Unified under Vertex AI client authentication (ADC / Service Account keys / Project + Location).
5. **Kimi / Moonshot** (`kimi`)
   - OpenAI-compatible adapter removed.
6. **MiniMax** (`minimax`)
   - OpenAI-compatible adapter removed.

## Active Provider
- **Vertex AI (`vertex`)**:
  - Uses Google ADK and `google.golang.org/genai`.
  - Dynamic model discovery from Vertex AI / Gemini API (queried live, no hardcoding).
  - Native streaming of thoughts/reasoning and content deltas (`genai.Part.Thought`).
  - Native function calling tool declarations (`genai.FunctionDeclaration` / `genai.Tool`) and session state management.
