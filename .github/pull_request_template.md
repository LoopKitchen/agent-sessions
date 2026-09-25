**What this changes and why**

**How you verified it**

Paste the relevant test output if CI does not obviously cover it.

**Checklist**

- [ ] `make lint && go test -race ./...` passes locally
- [ ] New behaviour has a test; scrub fixtures are synthetic
- [ ] Conventional commit subject; the body says why
- [ ] No company-specific identifiers, personal data or transcript content (the identifier gate will check the mechanical part)
- [ ] If the release layout or a schema constant changed, all the places that share the contract changed together
