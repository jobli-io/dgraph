/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package schema

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/hypermodeinc/dgraph/v25/graphql/authorization"
	"github.com/hypermodeinc/dgraph/v25/x"
)

var (
	defaultHttpClient = &http.Client{Timeout: time.Minute}
)

// graphqlResp represents a GraphQL response returned from a @custom(http: {...}) endpoint.
type graphqlResp struct {
	Errors x.GqlErrorList         `json:"errors,omitempty"`
	Data   map[string]interface{} `json:"data,omitempty"`
}

// MakeHttpRequest sends an HTTP request using the provided inputs. It returns the HTTP response
// along with any errors that were encountered.
// If no client is provided, it uses the defaultHttpClient which has a timeout of 1 minute.
func MakeHttpRequest(client *http.Client, method, url string, header http.Header,
	body []byte) (*http.Response, error) {
	var reqBody io.Reader
	if len(body) == 0 {
		reqBody = http.NoBody
	} else {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header = header

	if client == nil {
		client = defaultHttpClient
	}
	return client.Do(req)
}

// MakeAndDecodeHTTPRequest sends an HTTP request using the given url and body and then decodes the
// response correctly based on whether it was a GraphQL or REST request.
// It returns the decoded response along with either soft or hard errors.
// Soft error means one can assume that the response is valid and continue the normal execution.
// Hard error means there will be no response returned and one must stop the normal execution flow.
// For GraphQL requests, the GraphQL errors returned from the remote endpoint are considered soft
// errors. Any other kind of error is a hard error.
// For REST requests, any error is a hard error, including those returned from the remote endpoint.
func (fconf *FieldHTTPConfig) MakeAndDecodeHTTPRequest(client *http.Client, url string,
	body interface{}, field Field) (interface{}, x.GqlErrorList, x.GqlErrorList) {
	var b []byte
	var err error
	// need this check to make sure that we don't send body as []byte(`null`)
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return nil, nil, x.GqlErrorList{jsonMarshalError(err, field, body)}
		}
	}

	// Make the request to external HTTP endpoint using the URL and body
	resp, err := MakeHttpRequest(client, fconf.Method, url, fconf.ForwardHeaders, b)
	if err != nil {
		return nil, nil, x.GqlErrorList{externalRequestError(err, field)}
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			glog.Warningf("error closing body: %v", err)
		}
	}()
	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, x.GqlErrorList{externalRequestError(err, field)}
	}

	// Decode the HTTP response
	var response interface{}
	var softErrs x.GqlErrorList
	graphqlResp := graphqlResp{}
	if fconf.RemoteGqlQueryName != "" {
		// if it was a GraphQL request we need to decode the response as a GraphQL response
		if err = Unmarshal(b, &graphqlResp); err != nil {
			return nil, nil, x.GqlErrorList{jsonUnmarshalError(err, field)}
		}
		// if the GraphQL response has any errors, save them to be reported later
		softErrs = graphqlResp.Errors

		// find out the data returned for the GraphQL query
		var ok bool
		if response, ok = graphqlResp.Data[fconf.RemoteGqlQueryName]; !ok {
			return nil, nil, append(softErrs, keyNotFoundError(field, fconf.RemoteGqlQueryName))
		}
	} else {
		// this was a REST request
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// if this was a successful request, lets try to unmarshal the response
			if err = Unmarshal(b, &response); err != nil {
				return nil, nil, x.GqlErrorList{jsonUnmarshalError(err, field)}

			}
		} else {
			// if we get unsuccessful response from the REST api, lets try to see if
			// it sent any errors in the form expected for GraphQL errors.
			if err = Unmarshal(b, &graphqlResp); err != nil {
				// Fallback: if body is plain-text (or not valid JSON), wrap the trimmed raw body as the error message.
				rawBody := string(bytes.TrimSpace(b))
				if strings.HasPrefix(rawBody, "<!DOCTYPE html>") {
					preStart := strings.Index(rawBody, "<pre>")
					preEnd := strings.Index(rawBody, "</pre>")
					if preStart != -1 && preEnd != -1 && preEnd > preStart {
						rawBody = rawBody[preStart+5 : preEnd]
						rawBody = strings.ReplaceAll(rawBody, "&#39;", "'")
						rawBody = strings.ReplaceAll(rawBody, "&quot;", "\"")
						rawBody = strings.ReplaceAll(rawBody, "&lt;", "<")
						rawBody = strings.ReplaceAll(rawBody, "&gt;", ">")
						rawBody = strings.ReplaceAll(rawBody, "&amp;", "&")
						rawBody = strings.ReplaceAll(rawBody, "&nbsp;", " ")
						rawBody = strings.ReplaceAll(rawBody, "<br>", "\n")
					}
				}
				rawBody = strings.TrimSpace(rawBody)
				if rawBody == "" {
					rawBody = fmt.Sprintf("unexpected error with: %v", resp.StatusCode)
				} else {
					// If the error message contains multiple lines (e.g. stack trace), isolate the first line.
					if idx := strings.Index(rawBody, "\n"); idx != -1 {
						rawBody = strings.TrimSpace(rawBody[:idx])
					}
					// Strip standard 'Error: ' JS prefix to clean up the message
					rawBody = strings.TrimPrefix(rawBody, "Error: ")
					rawBody = strings.TrimSpace(rawBody)
				}
				if len(rawBody) > 500 {
					rawBody = rawBody[:500] + "..."
				}
				return nil, nil, x.GqlErrorList{field.GqlErrorf(nil, "%s", rawBody)}
			} else {
				return nil, nil, graphqlResp.Errors
			}
		}
	}

	return response, softErrs, nil
}

func keyNotFoundError(f Field, key string) *x.GqlError {
	return f.GqlErrorf(nil, "Evaluation of custom field failed because key: %s "+
		"could not be found in the JSON response returned by external request "+
		"for field: %s within type: %s.", key, f.Name(), f.GetObjectName())
}

func jsonMarshalError(err error, f Field, input interface{}) *x.GqlError {
	return f.GqlErrorf(nil, "Evaluation of custom field failed because json marshaling "+
		"(of: %+v) returned an error: %s for field: %s within type: %s.", input, err, f.Name(),
		f.GetObjectName())
}

func jsonUnmarshalError(err error, f Field) *x.GqlError {
	return f.GqlErrorf(nil, "Evaluation of custom field failed because json unmarshalling"+
		" result of external request failed (with error: %s) for field: %s within type: %s.",
		err, f.Name(), f.GetObjectName())
}

func externalRequestError(err error, f Field) *x.GqlError {
	return f.GqlErrorf(nil, "Evaluation of custom field failed because external request"+
		" returned an error: %s for field: %s within type: %s.", err, f.Name(), f.GetObjectName())
}

func GetBodyForLambda(ctx context.Context, field Field, parents,
	args interface{}) map[string]interface{} {
	accessJWT, _ := x.ExtractJwt(ctx)
	body := map[string]interface{}{
		"resolver":             field.GetObjectName() + "." + field.Name(),
		"X-Dgraph-AccessToken": accessJWT,
		"authHeader": map[string]interface{}{
			"key":   field.GetAuthMeta().GetHeader(),
			"value": authorization.GetJwtToken(ctx),
		},
	}
	if parents != nil {
		body["parents"] = parents
	}
	if args != nil {
		body["args"] = args
	} else {
		body["args"] = field.Arguments()
	}
	return body
}
