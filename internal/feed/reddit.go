package feed

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type subredditResponseJson struct {
	Data struct {
		Children []struct {
			Data struct {
				Id            string  `json:"id"`
				Title         string  `json:"title"`
				Upvotes       int     `json:"ups"`
				Url           string  `json:"url"`
				Time          float64 `json:"created"`
				CommentsCount int     `json:"num_comments"`
				Domain        string  `json:"domain"`
				Permalink     string  `json:"permalink"`
				Stickied      bool    `json:"stickied"`
				Pinned        bool    `json:"pinned"`
				IsSelf        bool    `json:"is_self"`
				Thumbnail     string  `json:"thumbnail"`
				Flair         string  `json:"link_flair_text"`
				ParentList    []struct {
					Id        string `json:"id"`
					Subreddit string `json:"subreddit"`
					Permalink string `json:"permalink"`
				} `json:"crosspost_parent_list"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type redditAccessTokenResponseJson struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
	TokenType   string `json:"token_type"`
}

type RedditOauth struct {
	ClientId     string    `yaml:"client-id"`
	ClientSecret string    `yaml:"client-secret"`
	Username     string    `yaml:"username"`
	Password     string    `yaml:"password"`
	UserAgent    string    `yaml:"user-agent"`
	AccessToken  string    `yaml:"-"`
	ExpiresAt    time.Time `yaml:"-"`
}

func (r *RedditOauth) ShouldAuthenticate() bool {
	return r.ClientId != "" && r.ClientSecret != "" && r.Username != "" && r.Password != ""
}

func (r *RedditOauth) ShouldReauthenticate() bool {
	if r.ExpiresAt.IsZero() {
		return false
	}
	return !r.ExpiresAt.After(time.Now())
}

func templateRedditCommentsURL(template, subreddit, postId, postPath string) string {
	template = strings.ReplaceAll(template, "{SUBREDDIT}", subreddit)
	template = strings.ReplaceAll(template, "{POST-ID}", postId)
	template = strings.ReplaceAll(template, "{POST-PATH}", strings.TrimLeft(postPath, "/"))

	return template
}

func TryAuthenticate(oauth *RedditOauth) error {
	if oauth.ClientId != "" && oauth.ClientSecret != "" && oauth.Username != "" && oauth.Password != "" {
		body := strings.NewReader(fmt.Sprintf(`grant_type=password&username=%s&password=%s`, oauth.Username, oauth.Password))
		request, err := http.NewRequest("POST", "https://www.reddit.com/api/v1/access_token", body)
		if err != nil {
			return err
		}
		request.SetBasicAuth(oauth.ClientId, oauth.ClientSecret)
		response, err := decodeJsonFromRequest[redditAccessTokenResponseJson](defaultClient, request)

		if err != nil {
			return err
		}

		if response.AccessToken == "" {
			return errors.New("reddit access token undefined")
		}

		if response.ExpiresIn == 0 {
			return errors.New("reddit expires in undefined")
		}

		oauth.AccessToken = response.AccessToken
		oauth.ExpiresAt = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)
	}

	return nil
}

func FetchSubredditPosts(subreddit, sort, topPeriod, search, commentsUrlTemplate, requestUrlTemplate string, showFlairs bool, oauth *RedditOauth) (ForumPosts, error) {
	query := url.Values{}
	var baseRequestUrl string
	var requestUrl string

	useOauth := oauth != nil && oauth.AccessToken != ""
	if useOauth {
		baseRequestUrl = "https://oauth.reddit.com"
		if oauth.ShouldReauthenticate() {
			err := TryAuthenticate(oauth)
			if err != nil {
				return nil, err
			}
		}
	} else {
		baseRequestUrl = "https://reddit.com"
	}

	if search != "" {
		query.Set("q", search+" subreddit:"+subreddit)
		query.Set("sort", sort)
	}

	if sort == "top" {
		query.Set("t", topPeriod)
	}

	if search != "" {
		requestUrl = fmt.Sprintf("%s/search.json?%s", baseRequestUrl, query.Encode())
	} else {
		requestUrl = fmt.Sprintf("%s/r/%s/%s.json?%s", baseRequestUrl, subreddit, sort, query.Encode())
	}

	if requestUrlTemplate != "" {
		requestUrl = strings.ReplaceAll(requestUrlTemplate, "{REQUEST-URL}", requestUrl)
	}

	request, err := http.NewRequest("GET", requestUrl, nil)

	if err != nil {
		return nil, err
	}

	if useOauth {
		var userAgent string
		if oauth.UserAgent != "" {
			userAgent = oauth.UserAgent
		} else {
			userAgent = fmt.Sprintf("glance/0.1 by %s", oauth.Username)
		}
		request.Header.Set("Authorization", fmt.Sprintf("bearer %s", oauth.AccessToken))
		request.Header.Set("User-Agent", userAgent)
	} else {
		// Required to increase rate limit, otherwise Reddit randomly returns 429 even after just 2 requests
		addBrowserUserAgentHeader(request)
	}

	responseJson, err := decodeJsonFromRequest[subredditResponseJson](defaultClient, request)

	if err != nil {
		return nil, err
	}

	if len(responseJson.Data.Children) == 0 {
		return nil, fmt.Errorf("no posts found")
	}

	posts := make(ForumPosts, 0, len(responseJson.Data.Children))

	for i := range responseJson.Data.Children {
		post := &responseJson.Data.Children[i].Data

		if post.Stickied || post.Pinned {
			continue
		}

		var commentsUrl string

		if commentsUrlTemplate == "" {
			commentsUrl = "https://www.reddit.com" + post.Permalink
		} else {
			commentsUrl = templateRedditCommentsURL(commentsUrlTemplate, subreddit, post.Id, post.Permalink)
		}

		forumPost := ForumPost{
			Title:           html.UnescapeString(post.Title),
			DiscussionUrl:   commentsUrl,
			TargetUrlDomain: post.Domain,
			CommentCount:    post.CommentsCount,
			Score:           post.Upvotes,
			TimePosted:      time.Unix(int64(post.Time), 0),
		}

		if post.Thumbnail != "" && post.Thumbnail != "self" && post.Thumbnail != "default" && post.Thumbnail != "nsfw" {
			forumPost.ThumbnailUrl = html.UnescapeString(post.Thumbnail)
		}

		if !post.IsSelf {
			forumPost.TargetUrl = post.Url
		}

		if showFlairs && post.Flair != "" {
			forumPost.Tags = append(forumPost.Tags, post.Flair)
		}

		if len(post.ParentList) > 0 {
			forumPost.IsCrosspost = true
			forumPost.TargetUrlDomain = "r/" + post.ParentList[0].Subreddit

			if commentsUrlTemplate == "" {
				forumPost.TargetUrl = "https://www.reddit.com" + post.ParentList[0].Permalink
			} else {
				forumPost.TargetUrl = templateRedditCommentsURL(
					commentsUrlTemplate,
					post.ParentList[0].Subreddit,
					post.ParentList[0].Id,
					post.ParentList[0].Permalink,
				)
			}
		}

		posts = append(posts, forumPost)
	}

	return posts, nil
}
