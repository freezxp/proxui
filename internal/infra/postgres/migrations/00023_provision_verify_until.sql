-- +goose Up

-- How long to keep waiting for a started guest's agent (PROV-16).
--
-- A guest that does not boot leaves every provisioning step reporting success:
-- the clone worked, the configuration was accepted, the start task finished,
-- and the platform says the machine is running. The one thing that never
-- happens is the guest agent answering, and until now that signal was thrown
-- away — an AlmaLinux 10 template built for a processor the guest was not given
-- panicked before init and looked, from every angle the portal could see,
-- exactly like a healthy machine.
--
-- Nullable, and only ever set while a request is in `verifying`: a build, a
-- destruction, and a guest nobody asked to start never wait for an agent.
ALTER TABLE provision_requests ADD COLUMN verify_until timestamptz;

-- +goose Down
ALTER TABLE provision_requests DROP COLUMN IF EXISTS verify_until;
