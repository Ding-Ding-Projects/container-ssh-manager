# Design route and implementation handoff

The locally available Material Designer handoff registry is documented as a read-only inventory for that product's existing component and design-token mappings, with installed runtime and visual parity unverified. It does not provide a verified create/export flow for this new target. The implementation uses registered Material Web components directly in the target's actual TypeScript frontend. No exported prototype is claimed as product evidence.

The Sites hosted runtime is Cloudflare Workers with a 128 MB isolate boundary. It cannot run the requested Go process, mount a local Docker socket, or host this Linux Compose service. Production delivery therefore uses the explicitly requested self-hosted stack.

## Screen inventory

Login; Overview; Hosts; Containers; Compose; Images; Volumes; Networks; Files; Tunnels; Commands; Schedules; Settings. Each screen must identify the current host when an action has a host target. Dialog states include empty, loading, validation, submitting, success, cancellation and recoverable failure where applicable.

## Verification tuple

Desktop 1440x1000 and compact 320x844, English/Cantonese/bilingual, light/dark, display scales 1/1.25/1.5/2. Production captures, semantic interaction results and geometry checks must bind to the source commit and built image digest. No raw visual reference is checked in yet; this handoff is a specification, not a visual parity claim.
