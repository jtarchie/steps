package resource

// The HTTP half of the linear-* types: authentication, retries, and GraphQL's "200 with errors". Nothing here knows what an issue is; linear.go does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

var errLinearToken = errors.New("no Linear API key")

// linearPersonalKeyPrefix is what a personal API key starts with, the one prefix Linear documents. Linear wants a personal key bare and an OAuth token as Bearer, so anything else is taken for OAuth: a personal key is then always right, and an OAuth token is right whatever it starts with.
const linearPersonalKeyPrefix = "lin_api_"

type linearClient struct {
	endpoint string
	token    string
}

// newLinearClient reads the key from the resource's resolved secrets first, so token_env may name a secrets: key, and from the daemon's environment otherwise.
func newLinearClient(ctx context.Context, connection config.LinearConnection) (*linearClient, error) {
	token, ok := shell.SecretEnv(ctx)[connection.Token()]
	if !ok {
		token = os.Getenv(connection.Token())
	}

	if token == "" {
		return nil, fmt.Errorf("%w: $%s is not set (source.token_env)", errLinearToken, connection.Token())
	}

	return &linearClient{endpoint: connection.API(), token: token}, nil
}

// authorization is the header value for the key: a personal key is sent bare, anything else as Bearer.
func (c *linearClient) authorization() string {
	if strings.HasPrefix(c.token, linearPersonalKeyPrefix) {
		return c.token
	}

	return "Bearer " + c.token
}

// query sends a read and decodes its data into out. A 5xx is sent again, as a GET's is.
func (c *linearClient) query(ctx context.Context, query string, variables map[string]any, out any) error {
	return c.do(ctx, http.MethodGet, query, variables, out)
}

// mutate sends a write. Only a 429 is sent again: a 502 after Linear accepted a comment would post it twice.
func (c *linearClient) mutate(ctx context.Context, mutation string, variables map[string]any, out any) error {
	return c.do(ctx, http.MethodPost, mutation, variables, out)
}

// do is one GraphQL request, always a POST on the wire; method only says which retry rules apply (see retryable).
func (c *linearClient) do(ctx context.Context, method, document string, variables map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": document, "variables": variables})
	if err != nil {
		return fmt.Errorf("linear: %w", err)
	}

	for attempt := 0; ; attempt++ {
		reply, wait, err := c.once(ctx, payload)
		if err != nil {
			return err
		}

		if worthRetrying(method, reply.status, wait, attempt) {
			err = sleepCtx(ctx, wait)
			if err != nil {
				return fmt.Errorf("linear: %w", err)
			}

			continue
		}

		return decodeLinear(reply, out)
	}
}

func (c *linearClient) once(ctx context.Context, payload []byte) (githubReply, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return githubReply{}, 0, fmt.Errorf("linear: %w", err)
	}

	request.Header.Set("Authorization", c.authorization())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "steps")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return githubReply{}, 0, fmt.Errorf("linear: POST %s: %w", c.endpoint, err)
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, githubMaxBody+1))
	if err != nil {
		return githubReply{}, 0, fmt.Errorf("linear: reading the answer: %w", err)
	}

	if len(body) > githubMaxBody {
		return githubReply{}, 0, fmt.Errorf("linear: the answer is larger than %d bytes", githubMaxBody)
	}

	return githubReply{status: response.StatusCode, body: body}, retryDelay(response.Header, attemptBackoff), nil
}

// decodeLinear turns an answer into out, or into Linear's own words when it was not the one asked for: a 200 may carry errors and no data.
func decodeLinear(reply githubReply, out any) error {
	var shaped struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	decoded := json.Unmarshal(reply.body, &shaped) == nil

	if len(shaped.Errors) > 0 {
		messages := make([]string, len(shaped.Errors))
		for i, problem := range shaped.Errors {
			messages[i] = problem.Message
		}

		return fmt.Errorf("linear: %d %s: %s", reply.status, http.StatusText(reply.status), strings.Join(messages, "; "))
	}

	if reply.status != http.StatusOK || !decoded {
		return fmt.Errorf("linear: %d %s: %s", reply.status, http.StatusText(reply.status), truncate(strings.TrimSpace(string(reply.body))))
	}

	if out == nil {
		return nil
	}

	err := json.Unmarshal(shaped.Data, out)
	if err != nil {
		return fmt.Errorf("linear: the answer is not the shape asked for: %w", err)
	}

	return nil
}

func truncate(message string) string {
	const limit = 500

	if len(message) <= limit {
		return message
	}

	cut := limit
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}

	return message[:cut] + "…"
}
