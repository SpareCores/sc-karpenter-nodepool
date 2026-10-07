# Documentation guide

## Where should my document go?

The docs folder structure for this project is the following, with
explanations of what belongs in each section:

- **documentation-guide**: meta information about writing documentation
  for this repository
- **explanation**: understanding-oriented discussion — decisions,
  architecture rationale, goals and non-goals, why something exists
- **how-to-guides**: goal-oriented directions addressing a specific goal
  or problem (including developer procedures such as testing and releasing)
- **reference**: technical description of how specific things within this
  project work (the `SCNodePool` API, selectors, provider adapters, flags)
- **tutorials**: learning-oriented experiences to get you up to speed
- **plans**: sequenced implementation plans under `plans/vX/{wip,done}`,
  with a companion index per version

## Conventions

- Reference code and files by their path relative to the repository root.
- Reference other projects with stable GitHub permalinks
  (`https://github.com/<org>/<repo>/blob/<full-sha>/<path>#L<n>`), never with
  local paths.
- Date-stamp facts that come from external data (Spare Cores data dumps,
  provider releases), as they go stale.

## Documentation templates

Templates for this repository include:

- [Feature documentation template](./feature-documentation-template.md)
