package endpoint

import "errors"

// ErrInvalidVisibility is the error returned when an endpoint, external endpoint or
// suite is configured with a visibility other than "public" or "private"
var ErrInvalidVisibility = errors.New("invalid visibility: must be 'public' or 'private'")

// Visibility determines whether an endpoint or suite is displayed on tenant-scoped
// (subdomain) views of the dashboard when multi-tenancy is enabled. It has no effect
// on the apex/unscoped view, which always displays everything, nor on deployments
// without a tenancy configuration. See docs/multi-tenancy.md.
type Visibility string

const (
	// VisibilityPublic makes the endpoint visible on its group's tenant subdomain
	VisibilityPublic Visibility = "public"

	// VisibilityPrivate hides the endpoint from tenant subdomains (default)
	VisibilityPrivate Visibility = "private"
)

// ValidateAndSetDefault validates the visibility, defaulting to VisibilityPrivate
// when unset so that internal endpoints are never exposed to a tenant unless
// explicitly marked public.
func (v *Visibility) ValidateAndSetDefault() error {
	switch *v {
	case "":
		*v = VisibilityPrivate
		return nil
	case VisibilityPublic, VisibilityPrivate:
		return nil
	default:
		return ErrInvalidVisibility
	}
}

// IsPublic returns whether the visibility is public
func (v Visibility) IsPublic() bool {
	return v == VisibilityPublic
}
