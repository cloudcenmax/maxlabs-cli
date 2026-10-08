import assert from "node:assert/strict";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { OAuthSession } from "../src/auth.ts";

const future = "2999-01-01T00:00:00.000Z";

function account(id: string, name: string) {
  const organizationId = id === "personal" ? null : Number(id.split(":")[1]);
  return {
    id,
    type: id === "personal" ? "personal" : "organization",
    name,
    organization_id: organizationId,
    membership_id: organizationId ? organizationId + 100 : null,
    available: true,
  };
}

function record(id: string, name: string, token: string) {
  return {
    account: account(id, name),
    tokens: { access_token: token, refresh_token: `refresh-${token}`, token_type: "Bearer", expires_at: future },
  };
}

async function storePath(): Promise<string> {
  return join(await mkdtemp(join(tmpdir(), "maxlabs-cli-auth-")), "oauth.json");
}

test("switching selects a complete stored token context without rewriting either token", async (t) => {
  const path = await storePath();
  await writeFile(path, JSON.stringify({
    version: 2,
    active_account_id: "personal",
    accounts: {
      personal: record("personal", "Personal", "personal-token"),
      "organization:7": record("organization:7", "Acme", "work-token"),
    },
  }));
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  globalThis.fetch = async () => Response.json({
    current_account_id: "personal",
    accounts: [account("personal", "Personal"), account("organization:7", "Acme")],
  });
  const oauth = new OAuthSession("https://gateway.test", path);

  assert.equal(await oauth.accessToken(), "personal-token");
  assert.equal((await oauth.switchAccount("Acme")).id, "organization:7");
  assert.equal(await oauth.accessToken(), "work-token");

  const saved = JSON.parse(await readFile(path, "utf8"));
  assert.equal(saved.active_account_id, "organization:7");
  assert.equal(saved.accounts.personal.tokens.access_token, "personal-token");
  assert.equal(saved.accounts["organization:7"].tokens.access_token, "work-token");
});

test("account discovery marks connected contexts and migrates the legacy single-token store", async (t) => {
  const path = await storePath();
  await writeFile(path, JSON.stringify({
    access_token: "legacy-work-token",
    refresh_token: "legacy-refresh",
    token_type: "Bearer",
    expires_at: future,
  }));
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  globalThis.fetch = async (_input, init) => {
    assert.equal(new Headers(init?.headers).get("authorization"), "Bearer legacy-work-token");
    return Response.json({
      current_account_id: "organization:7",
      accounts: [account("personal", "Personal"), account("organization:7", "Acme")],
    });
  };

  const oauth = new OAuthSession("https://gateway.test", path);
  const accounts = await oauth.accounts();
  assert.deepEqual(accounts.map(({ id, connected, active }) => ({ id, connected, active })), [
    { id: "personal", connected: false, active: false },
    { id: "organization:7", connected: true, active: true },
  ]);
  assert.equal(await oauth.accessToken(), "legacy-work-token");
  const saved = JSON.parse(await readFile(path, "utf8"));
  assert.equal(saved.accounts.legacy, undefined);
  assert.equal(saved.accounts["organization:7"].tokens.refresh_token, "legacy-refresh");
});

test("device login verifies the token account before saving and preserves existing accounts on failure", async (t) => {
  const path = await storePath();
  await writeFile(path, JSON.stringify({
    version: 2,
    active_account_id: "personal",
    accounts: { personal: record("personal", "Personal", "personal-token") },
  }));
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  let request = 0;
  globalThis.fetch = async () => {
    request++;
    if (request === 1) return Response.json({
      device_code: "device",
      user_code: "ABCD-EFGH",
      verification_uri: "https://gateway.test/oauth/device",
      expires_in: 5,
      interval: 1,
    });
    if (request === 2) return Response.json({
      access_token: "unverified-work-token",
      refresh_token: "unverified-refresh",
      token_type: "Bearer",
      expires_in: 3600,
    });
    return new Response("denied", { status: 403 });
  };

  const oauth = new OAuthSession("https://gateway.test", path);
  const output = { write() { return true; } } as unknown as NodeJS.WritableStream;
  await assert.rejects(oauth.login(output), /credentials were not saved/);
  assert.equal(await oauth.accessToken(), "personal-token");
  const saved = JSON.parse(await readFile(path, "utf8"));
  assert.deepEqual(Object.keys(saved.accounts), ["personal"]);
});

test("removing the active account falls back to another stored context", async () => {
  const path = await storePath();
  await writeFile(path, JSON.stringify({
    version: 2,
    active_account_id: "organization:7",
    accounts: {
      personal: record("personal", "Personal", "personal-token"),
      "organization:7": record("organization:7", "Acme", "work-token"),
    },
  }));
  const oauth = new OAuthSession("https://gateway.test", path);
  const next = await oauth.removeAccount();
  assert.equal(next?.id, "personal");
  assert.equal(await oauth.accessToken(), "personal-token");
});

test("a revoked active work session cannot lock the user out of a stored personal account", async (t) => {
  const path = await storePath();
  const expiredWork = record("organization:7", "Acme", "expired-work-token");
  expiredWork.tokens.expires_at = "2000-01-01T00:00:00.000Z";
  await writeFile(path, JSON.stringify({
    version: 2,
    active_account_id: "organization:7",
    accounts: {
      personal: record("personal", "Personal", "personal-token"),
      "organization:7": expiredWork,
    },
  }));
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  globalThis.fetch = async () => Response.json({ error: "invalid_grant" }, { status: 400 });

  const oauth = new OAuthSession("https://gateway.test", path);
  assert.equal((await oauth.switchAccount("personal")).id, "personal");
  assert.equal(await oauth.accessToken(), "personal-token");
});
