package store

import (
	"context"
	"fmt"
)

// ProvisionSpec is one declaratively provisioned project (spec 001 #9).
type ProvisionSpec struct {
	Name      string
	PublicKey string
	SecretKey string
}

// BootstrapResult reports what Bootstrap did. Secrets are present only for
// projects created in this run; the store never returns stored secrets because
// it does not have them.
type BootstrapResult struct {
	Created []BootstrapCreated
}

// BootstrapCreated is one project Bootstrap created. Declared says its keys
// came from TRACEPAD_PROJECTS rather than being generated here: the operator
// already holds that secret, so it is not one to print (spec 001 #12).
// Scopes is what the key may do, spelled as the column stores it.
type BootstrapCreated struct {
	Project  Project
	Keys     KeyPair
	Declared bool
	Scopes   string
}

// Bootstrap provisions projects idempotently. Declared projects that already
// exist are left untouched (keys are never rotated from env). With no specs
// and an empty database it creates project "default" with a generated key that
// holds ingest alone: it is the one secret the start prints, to a log that
// outlives it, and an application is what it is printed for (spec 045 #28).
// A declared key holds all three (#5).
func (s *Store) Bootstrap(specs []ProvisionSpec) (*BootstrapResult, error) {
	res := &BootstrapResult{}

	declared := len(specs) > 0
	if !declared {
		n, err := s.CountProjects(context.Background())
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return res, nil
		}
		keys, err := GenerateKeyPair()
		if err != nil {
			return nil, err
		}
		specs = []ProvisionSpec{{Name: "default", PublicKey: keys.PublicKey, SecretKey: keys.Secret}}
	}

	for _, spec := range specs {
		existing, err := s.ProjectByName(context.Background(), spec.Name)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			// A declared project inside its deletion grace window is
			// not recreated — its name is reserved and its keys are
			// dead (spec 005 #9) — so say so rather than starting
			// with an ingest surface that silently 401s.
			if existing.Deleted() {
				logger().Warn("declared project is deleted and its keys will not authenticate; restore it",
					"project", spec.Name, "purge_at", existing.PurgeAt())
			}
			continue
		}
		scopes := AllScopes
		if !declared {
			scopes = ScopeIngest
		}
		p, err := s.createProject(spec.Name, KeyPair{PublicKey: spec.PublicKey, Secret: spec.SecretKey}, scopes)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
		res.Created = append(res.Created, BootstrapCreated{
			Project:  *p,
			Keys:     KeyPair{PublicKey: spec.PublicKey, Secret: spec.SecretKey},
			Declared: declared,
			Scopes:   scopes,
		})
	}
	return res, nil
}
