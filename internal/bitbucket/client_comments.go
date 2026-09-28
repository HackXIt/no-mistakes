package bitbucket

import (
	"context"
	"fmt"
	"net/http"
)

type PRComment struct {
	ID        int
	Body      string
	URL       string
	Principal string
}

const maxBitbucketPRCommentPages = 100

func (c *Client) AuthenticatedPrincipal(ctx context.Context) (string, error) {
	var user struct {
		UUID string `json:"uuid"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/2.0/user", nil, nil, &user); err != nil {
		return "", err
	}
	if user.UUID == "" {
		return "", fmt.Errorf("Bitbucket authenticated user response was incomplete")
	}
	return user.UUID, nil
}

func (c *Client) ListPRComments(ctx context.Context, repo RepoRef, prID int) ([]PRComment, error) {
	next := fmt.Sprintf("%s/%d/comments?pagelen=100", repoPRPath(repo), prID)
	comments := make([]PRComment, 0)
	visited := make(map[string]struct{})
	for page := 1; next != ""; page++ {
		if page > maxBitbucketPRCommentPages {
			return nil, fmt.Errorf("Bitbucket PR comment pagination exceeded %d pages", maxBitbucketPRCommentPages)
		}
		if _, ok := visited[next]; ok {
			return nil, fmt.Errorf("Bitbucket PR comment pagination repeated a page")
		}
		visited[next] = struct{}{}
		var response *struct {
			Values []bitbucketPRComment `json:"values"`
			Next   string               `json:"next"`
		}
		if err := c.doJSONPathOrURL(ctx, http.MethodGet, next, nil, &response); err != nil {
			return nil, err
		}
		if response == nil || response.Values == nil {
			return nil, fmt.Errorf("decode Bitbucket PR comments: expected values array")
		}
		for _, raw := range response.Values {
			if raw.Deleted {
				continue
			}
			comment, err := normalizeBitbucketPRComment(raw, 0)
			if err != nil {
				return nil, err
			}
			comments = append(comments, comment)
		}
		if response.Next != "" {
			validated, err := c.validatePaginationURL(response.Next)
			if err != nil {
				return nil, err
			}
			next = validated
		} else {
			next = ""
		}
	}
	return comments, nil
}

func (c *Client) CreatePRComment(ctx context.Context, repo RepoRef, prID int, body string) (PRComment, error) {
	request := map[string]any{"content": map[string]string{"raw": body}}
	var raw bitbucketPRComment
	endpoint := fmt.Sprintf("%s/%d/comments", repoPRPath(repo), prID)
	if err := c.doJSON(ctx, http.MethodPost, endpoint, nil, request, &raw); err != nil {
		return PRComment{}, err
	}
	comment, err := normalizeBitbucketPRComment(raw, 0)
	if err != nil {
		return PRComment{}, err
	}
	if comment.Body != body {
		return PRComment{}, fmt.Errorf("Bitbucket PR comment write did not preserve the proposed body")
	}
	return comment, nil
}

func (c *Client) UpdatePRComment(ctx context.Context, repo RepoRef, prID, commentID int, body string) (PRComment, error) {
	request := map[string]any{"content": map[string]string{"raw": body}}
	var raw bitbucketPRComment
	endpoint := fmt.Sprintf("%s/%d/comments/%d", repoPRPath(repo), prID, commentID)
	if err := c.doJSON(ctx, http.MethodPut, endpoint, nil, request, &raw); err != nil {
		return PRComment{}, err
	}
	comment, err := normalizeBitbucketPRComment(raw, commentID)
	if err != nil {
		return PRComment{}, err
	}
	if comment.Body != body {
		return PRComment{}, fmt.Errorf("Bitbucket PR comment write did not preserve the proposed body")
	}
	return comment, nil
}

type bitbucketPRComment struct {
	ID      int  `json:"id"`
	Deleted bool `json:"deleted"`
	User    struct {
		UUID string `json:"uuid"`
	} `json:"user"`
	Content *struct {
		Raw *string `json:"raw"`
	} `json:"content"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func normalizeBitbucketPRComment(raw bitbucketPRComment, expectedID int) (PRComment, error) {
	if raw.ID <= 0 || raw.Deleted || raw.Content == nil || raw.Content.Raw == nil {
		return PRComment{}, fmt.Errorf("Bitbucket PR comment response was incomplete")
	}
	if expectedID > 0 && raw.ID != expectedID {
		return PRComment{}, fmt.Errorf("Bitbucket PR comment identity mismatch: got %d, expected %d", raw.ID, expectedID)
	}
	return PRComment{ID: raw.ID, Body: *raw.Content.Raw, URL: raw.Links.HTML.Href, Principal: raw.User.UUID}, nil
}
