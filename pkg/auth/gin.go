package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GinRequireScope is the gin-flavoured version of RequireScope. Mount
// AFTER the gin auth adapter that propagates the scope context, e.g.:
//
//	authed := r.Group("/")
//	authed.Use(ginAuth(authMW))                       // propagates ctx.Request
//	authed.POST("/v1/query", auth.GinRequireScope(auth.ScopeQuery), handler)
//
// On insufficient scope the response is 403 with a WWW-Authenticate
// header listing the required scope(s) so clients can surface a
// human-readable error.
func GinRequireScope(required ...Scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		granted := ScopesFromContext(c.Request.Context())
		if !HasScope(granted, required...) {
			c.Header("WWW-Authenticate",
				`Bearer scope="`+joinScopes(required)+`"`)
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"error": "insufficient scope", "required": ScopesAsStrings(required)})
			return
		}
		c.Next()
	}
}

func joinScopes(in []Scope) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += " "
		}
		out += string(s)
	}
	return out
}
