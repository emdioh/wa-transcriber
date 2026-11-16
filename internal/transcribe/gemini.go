package transcribe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// gemini transcribes, detects the language and translates in a single
// generateContent call with inline audio and a JSON response schema.
type gemini struct {
	s    Settings
	http *http.Client
}

func geminiPrompt(s Settings) string {
	p := "Transcribe this voice note verbatim in its original language: no comments, " +
		"no timestamps, no speaker labels. Put the transcript in \"transcript\" (empty if " +
		"there is no speech) and the ISO 639-1 code of its language in \"language\"."
	if s.TranslateTo == "" {
		return p + ` Leave "translation" empty.`
	}
	keep := append([]string{s.TranslateTo}, s.Keep...)
	return p + " If the language is one of [" + strings.Join(keep, ", ") + `], leave "translation" ` +
		"empty; otherwise put in \"translation\" a faithful translation of the transcript into " +
		languageLabel(s.TranslateTo) + "."
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

func (g gemini) process(ctx context.Context, audio []byte) (Result, error) {
	str := map[string]string{"type": "STRING"}
	body := map[string]any{
		"contents": []any{map[string]any{"parts": []any{
			map[string]any{"text": geminiPrompt(g.s)},
			map[string]any{"inline_data": map[string]string{
				"mime_type": "audio/ogg",
				"data":      base64.StdEncoding.EncodeToString(audio),
			}},
		}}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
			"responseSchema": map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"language": str, "transcript": str, "translation": str,
				},
				"required": []string{"language", "transcript"},
			},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	url := g.s.BaseURL + "/models/" + g.s.Model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.s.APIKey)
	var resp geminiResponse
	if err := doJSON(g.http, req, g.s.APIKey, &resp); err != nil {
		return Result{}, err
	}
	if len(resp.Candidates) == 0 {
		if r := resp.PromptFeedback.BlockReason; r != "" {
			return Result{}, fmt.Errorf("blocked by Gemini (%s)", r)
		}
		return Result{}, nil
	}
	var sb strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	raw := strings.TrimSpace(sb.String())
	if raw == "" {
		switch r := resp.Candidates[0].FinishReason; r {
		case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
			return Result{}, fmt.Errorf("blocked by Gemini (%s)", r)
		}
		return Result{}, nil
	}
	var out struct {
		Language    string `json:"language"`
		Transcript  string `json:"transcript"`
		Translation string `json:"translation"`
	}
	if err := json.Unmarshal([]byte(stripFences(raw)), &out); err != nil {
		return Result{}, fmt.Errorf("Gemini answer is not JSON: %s", excerpt(raw, ""))
	}
	return Result{
		Text:        strings.TrimSpace(out.Transcript),
		Language:    NormalizeLanguage(out.Language),
		Translation: strings.TrimSpace(out.Translation),
	}, nil
}
