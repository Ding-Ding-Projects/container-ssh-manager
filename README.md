# Container SSH Manager

Private self-hosted container and SSH administration. Implementation in progress.

## Architecture contract

Go module: github.com/Ding-Ding-Projects/container-ssh-manager.
Packages own registration through Register(mux *http.ServeMux). All package handlers sit behind the main server authentication and same-origin middleware. GET /api/v1/health and POST /api/v1/login are the only public APIs.

Foundation owns internal/core and cmd/manager: core.Store with DB *sql.DB, Put(kind,id string,v any) error, Get(kind,id string,v any) error, List(kind string) ([]json.RawMessage,error), Delete(kind,id string) error; core.Vault Seal([]byte)([]byte,error), Open([]byte)([]byte,error). JSON records table supports transactional job storage through DB when needed. core.ID() string, core.JSON(w,status,v), core.Error(w,status,message), core.Decode(r,v) error. All JSON responses use direct objects/arrays; errors {error:string}.

Connection lane owns internal/connection. New(store *core.Store,vault *core.Vault) *Manager. Manager.Register(mux). Manager.Dial(ctx,hostID)(*ssh.Client,error), Manager.Run(ctx,hostID,command string)(int,error) discards output by default, Manager.EngineClient(ctx,hostID)(*http.Client,string,io.Closer,error). Local ID is local, local engine Unix socket path from DOCKER_SOCKET. Export Host {id,name,address,port,user,credentialId,hostKey,jumpIds,group,tags}. Register hosts/credentials, SFTP, terminals and tunnels APIs. Document exact routes in docs/api-connections.md for frontend.

Container lane owns internal/engine. New(store *core.Store, connections *connection.Manager) *Manager, Register(mux). Container, image, volume, network and Compose APIs. Document exact routes in docs/api-engine.md. Engine operations use EngineClient. Avoid shell interpolated user input; quote explicit Compose paths and pass controlled operations. Never permit arbitrary HTTP proxy URLs.

Jobs lane owns internal/jobs. New(store *core.Store,connections *connection.Manager,vault *core.Vault)*Manager, Register(mux), Start(ctx). Own snippets, schedules, runs and audit APIs. Document routes docs/api-jobs.md. Job intent durable before execution; four global slots, one schedule per host. No stored plaintext command output.

Frontend lane owns web only and docs/ui.md. TypeScript/Vite, Material Web components and xterm. Same-origin API. Read the three API documents as lanes create them. No fake data. Use actual controls for all scoped features. Production web/dist embedded by root server through web/assets.go added by main integrator.

Root integrator owns foundation, auth, packaging, docs index, integration tests, deployment, design route and review. Lanes may edit only their assigned package/document and may add their module requirements to docs/dependencies-<lane>.txt; main owns go.mod/go.sum. No secrets or private host data in source.
