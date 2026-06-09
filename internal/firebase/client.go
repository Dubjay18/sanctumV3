package firebase

import (
	"context"
	"net/http"
	"strings"

	firestore "cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	auth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

type ContextKey string

const (
	ContextKeyUID  ContextKey = "uid"
	ContextKeyName ContextKey = "name"
)

// InitFirebase initializes the Firebase Admin SDK using the credentials JSON at the specified path.
// It returns both a Firestore client and an Auth client.
func InitFirebase(credentialsPath string) (*firestore.Client, *auth.Client, error) {
	ctx := context.Background()
	opt := option.WithCredentialsFile(credentialsPath)
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		return nil, nil, err
	}
	fsClient, err := app.Firestore(ctx)
	if err != nil {
		return nil, nil, err
	}
	authClient, err := app.Auth(ctx)
	if err != nil {
		fsClient.Close()
		return nil, nil, err
	}
	return fsClient, authClient, nil
}

// AuthMiddleware wraps a handler to authenticate requests via Firebase ID Token (Bearer token in Authorization header).
func AuthMiddleware(authClient *auth.Client, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		token := parts[1]
		decodedToken, err := authClient.VerifyIDToken(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		name, _ := decodedToken.Claims["name"].(string)

		ctx := context.WithValue(r.Context(), ContextKeyUID, decodedToken.UID)
		ctx = context.WithValue(ctx, ContextKeyName, name)

		next(w, r.WithContext(ctx))
	}
}
