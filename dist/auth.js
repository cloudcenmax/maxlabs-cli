import { chmod, mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { homedir } from "node:os";

                  
                       
                        
                     
                     
 

                       
                      
                    
                           
                                     
                     
                   
 

                                 
             
                                    
               
                 
                                 
                                
                       
                                             
                     
 

                                                       
                     
                  
 

                         
                          
                 
 

                       
             
                             
                                          
 

                           
                              
                       
 

const legacyAccountId = "legacy";

export function defaultAuthStore()         {
  return join(homedir(), ".maxlabs", "oauth.json");
}

/** A multi-account OAuth vault. Tokens are never reassigned across accounts. */
export class OAuthSession {
  #store              = emptyStore();
  #loaded = false;
           baseUrl        ;
           storePath        ;

  constructor(baseUrl        , storePath = defaultAuthStore()) {
    this.baseUrl = baseUrl;
    this.storePath = storePath;
  }

  async signedIn()                   {
    await this.#load();
    return Boolean(this.#activeRecord()?.tokens.access_token || this.#activeRecord()?.tokens.refresh_token);
  }

  async activeAccount()                                      {
    await this.#load();
    const record = this.#activeRecord();
    return record?.account.id === legacyAccountId ? undefined : record?.account;
  }

  async storedAccounts()                           {
    await this.#load();
    return Object.values(this.#store.accounts)
      .filter((record) => record.account.id !== legacyAccountId)
      .map((record) => ({
        ...record.account,
        connected: true,
        active: record.account.id === this.#store.active_account_id,
      }));
  }

  async accounts(signal              )                           {
    const fallback = await this.storedAccounts();
    let token                    ;
    try { token = await this.accessToken(signal); }
    catch (error) {
      if (fallback.length) return fallback;
      throw error;
    }
    if (!token) return fallback;
    let response          ;
    try {
      response = await fetch(`${this.baseUrl}/app/v1/accounts`, {
        headers: { accept: "application/json", authorization: `Bearer ${token}` },
        signal,
      });
    } catch (error) {
      if (fallback.length) return fallback;
      throw error;
    }
    if (!response.ok) {
      if (fallback.length) return fallback;
      throw new Error(`Account discovery returned ${response.status}`);
    }
    const payload = await response.json()                   ;
    const available = (payload.accounts || []).map(normaliseAccount).filter((account)                            => Boolean(account));
    if (this.#store.active_account_id === legacyAccountId && payload.current_account_id) {
      const current = available.find((account) => account.id === payload.current_account_id);
      const legacy = this.#store.accounts[legacyAccountId];
      if (current && legacy) {
        this.#store.accounts[current.id] = { account: current, tokens: legacy.tokens };
        delete this.#store.accounts[legacyAccountId];
        this.#store.active_account_id = current.id;
      }
    }
    for (const account of available) {
      const stored = this.#store.accounts[account.id];
      if (stored) stored.account = account;
    }
    await this.#persist();
    const activeId = this.#store.active_account_id;
    const connected = new Set(Object.keys(this.#store.accounts));
    return available.map((account) => ({ ...account, connected: connected.has(account.id), active: account.id === activeId }));
  }

  async switchAccount(selector        )                          {
    await this.#load();
    const visible = await this.accounts();
    const selectedAccount = selectAccount(visible, selector);
    if (!selectedAccount) throw new Error(`No account matches ${JSON.stringify(selector)}.`);
    const selected = this.#store.accounts[selectedAccount.id];
    if (!selected) throw new Error(`${selectedAccount.name} is not connected yet. Use /account add and choose it in browser consent.`);
    if (!selected.account.available) throw new Error(`${selected.account.name} is no longer available. Remove it and ask the organization owner to restore your seat.`);
    this.#store.active_account_id = selected.account.id;
    await this.#persist();
    return selected.account;
  }

  async removeAccount(selector = "")                                      {
    await this.#load();
    const selectedAccount = selector ? selectAccount(await this.accounts(), selector) : undefined;
    const selected = selector ? this.#store.accounts[selectedAccount?.id || ""] : this.#activeRecord();
    if (!selected) throw new Error(selector ? `No connected account matches ${JSON.stringify(selector)}.` : "No account is connected.");
    delete this.#store.accounts[selected.account.id];
    if (this.#store.active_account_id === selected.account.id) {
      this.#store.active_account_id = Object.keys(this.#store.accounts)[0];
    }
    await this.#persist();
    return this.#activeRecord()?.account;
  }

  /** Existing logout semantics: clear every saved OAuth identity. */
  async logout()                {
    await this.#load();
    this.#store = emptyStore();
    await this.#persist();
  }

  async accessToken(signal              )                              {
    await this.#load();
    const accountId = this.#store.active_account_id;
    const record = this.#activeRecord();
    if (!record || !accountId) return undefined;
    const expiresAt = Date.parse(record.tokens.expires_at);
    if (record.tokens.access_token && expiresAt > Date.now() + 60_000) return record.tokens.access_token;
    if (!record.tokens.refresh_token) return undefined;

    let tokens        ;
    try {
      tokens = await this.#exchange(new URLSearchParams({
        grant_type: "refresh_token",
        client_id: "cli",
        refresh_token: record.tokens.refresh_token,
      }), signal);
    } catch (error) {
      if (!(error instanceof OAuthRefreshError) || error.code !== "invalid_grant") throw error;
      // A revoked or already-rotated refresh token cannot recover. Forget only
      // its owner, then use another connected account when one is available.
      delete this.#store.accounts[accountId];
      if (this.#store.active_account_id === accountId) {
        this.#store.active_account_id = Object.keys(this.#store.accounts)[0];
      }
      await this.#persist();
      return this.accessToken(signal);
    }
    // Save against the owner of the rotating refresh token, even if selection
    // changed while the network request was in flight.
    if (this.#store.accounts[accountId]) this.#store.accounts[accountId].tokens = tokens;
    await this.#persist();
    if (this.#store.active_account_id !== accountId) return this.accessToken(signal);
    return tokens.access_token;
  }

  async login(output                       , signal              )                          {
    const response = await fetch(`${this.baseUrl}/oauth/device/code`, {
      method: "POST",
      headers: { accept: "application/json", "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ client_id: "cli", scope: "profile usage:read chat" }),
      signal,
    });
    if (!response.ok) throw new Error(`OAuth device authorization returned ${response.status}`);
    const grant = await response.json()               ;
    output.write(`\n  Open ${grant.verification_uri_complete || grant.verification_uri}\n`);
    output.write(`  Confirm code ${grant.user_code} and choose Personal or a work account\n\n`);

    let interval = Math.max(grant.interval || 1, 1) * 1_000;
    const deadline = Date.now() + grant.expires_in * 1_000;
    while (Date.now() < deadline) {
      await abortableDelay(interval, signal);
      const poll = await fetch(`${this.baseUrl}/oauth/token`, {
        method: "POST",
        headers: { accept: "application/json", "content-type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({
          grant_type: "urn:ietf:params:oauth:grant-type:device_code",
          client_id: "cli",
          device_code: grant.device_code,
        }),
        signal,
      });
      const payload = await poll.json()                           ;
      if (payload.error === "authorization_pending") continue;
      if (payload.error === "slow_down") { interval += 5_000; continue; }
      if (!poll.ok || payload.error) throw new Error(`OAuth authorization failed: ${payload.error || poll.status}`);
      const tokens = normaliseTokens(payload);
      const account = await this.#profile(tokens.access_token, signal);
      await this.#load();
      this.#store.accounts[account.id] = { account, tokens };
      delete this.#store.accounts[legacyAccountId];
      this.#store.active_account_id = account.id;
      await this.#persist();
      return account;
    }
    throw new Error("OAuth device code expired");
  }

  async #profile(token        , signal              )                          {
    const response = await fetch(`${this.baseUrl}/app/v1/me`, {
      headers: { accept: "application/json", authorization: `Bearer ${token}` },
      signal,
    });
    if (!response.ok) throw new Error(`OAuth account verification returned ${response.status}; credentials were not saved`);
    const payload = await response.json()                           ;
    const account = normaliseAccount(payload.account);
    if (account) return account;
    // Compatibility while clients and gateway roll out independently.
    const organizationId = nullablePositiveInteger(payload.organization_id);
    return {
      id: organizationId === null ? "personal" : `organization:${organizationId}`,
      type: organizationId === null ? "personal" : "organization",
      name: organizationId === null ? String(payload.name || "Personal") : `Organization ${organizationId}`,
      email: typeof payload.email === "string" ? payload.email : undefined,
      organization_id: organizationId,
      tier: normaliseTier(payload.tier),
      available: true,
    };
  }

  async #exchange(form                 , signal              )                  {
    const response = await fetch(`${this.baseUrl}/oauth/token`, {
      method: "POST",
      headers: { accept: "application/json", "content-type": "application/x-www-form-urlencoded" },
      body: form,
      signal,
    });
    const payload = await response.json()                           ;
    if (!response.ok || payload.error) throw new OAuthRefreshError(String(payload.error || response.status));
    return normaliseTokens(payload);
  }

  async #load()                {
    if (this.#loaded) return;
    this.#loaded = true;
    try {
      const parsed = JSON.parse(await readFile(this.storePath, "utf8"))                                 ;
      if (parsed.version === 2 && parsed.accounts && typeof parsed.accounts === "object") {
        this.#store = { version: 2, active_account_id: parsed.active_account_id, accounts: parsed.accounts };
        if (!this.#store.accounts[this.#store.active_account_id || ""]) this.#store.active_account_id = Object.keys(this.#store.accounts)[0];
      } else if (parsed.access_token || parsed.refresh_token) {
        this.#store = {
          version: 2,
          active_account_id: legacyAccountId,
          accounts: {
            [legacyAccountId]: {
              account: { id: legacyAccountId, type: "personal", name: "Existing OAuth session", organization_id: null, available: true },
              tokens: parsed          ,
            },
          },
        };
      }
    } catch (error) {
      if ((error                         ).code !== "ENOENT") throw error;
    }
  }

  #activeRecord()                            {
    return this.#store.accounts[this.#store.active_account_id || ""];
  }

  async #persist()                {
    await mkdir(dirname(this.storePath), { recursive: true, mode: 0o700 });
    const temporary = `${this.storePath}.${process.pid}.${Date.now()}.tmp`;
    await writeFile(temporary, `${JSON.stringify(this.#store)}\n`, { mode: 0o600 });
    await chmod(temporary, 0o600);
    await rename(temporary, this.storePath);
  }
}

class OAuthRefreshError extends Error {
           code        ;

  constructor(code        ) {
    super(`OAuth refresh failed: ${code}`);
    this.code = code;
  }
}

function emptyStore()              {
  return { version: 2, accounts: {} };
}

function selectAccount                          (accounts     , selector        )                {
  const number = Number(selector);
  if (Number.isInteger(number) && number > 0) return accounts[number - 1];
  const wanted = selector.trim().toLocaleLowerCase();
  const exact = accounts.filter((account) => account.id.toLocaleLowerCase() === wanted || account.name.toLocaleLowerCase() === wanted);
  if (exact.length === 1) return exact[0];
  const partial = accounts.filter((account) => account.name.toLocaleLowerCase().includes(wanted));
  return partial.length === 1 ? partial[0] : undefined;
}

function normaliseAccount(value         )                             {
  if (!value || typeof value !== "object") return undefined;
  const raw = value                           ;
  const organizationId = nullablePositiveInteger(raw.organization_id);
  const type = raw.type === "organization" ? "organization" : raw.type === "personal" ? "personal" : undefined;
  const id = typeof raw.id === "string" ? raw.id.trim() : "";
  if (!id || !type) return undefined;
  if (type === "personal" && (id !== "personal" || organizationId !== null)) return undefined;
  if (type === "organization" && (organizationId === null || id !== `organization:${organizationId}`)) return undefined;
  return {
    id,
    type,
    name: String(raw.name || (type === "personal" ? "Personal" : `Organization ${organizationId}`)),
    email: typeof raw.email === "string" ? raw.email : undefined,
    organization_id: organizationId,
    membership_id: nullablePositiveInteger(raw.membership_id),
    role: typeof raw.role === "string" ? raw.role : null,
    tier: normaliseTier(raw.tier),
    available: raw.available !== false,
  };
}

function normaliseTier(value         )                                      {
  if (!value || typeof value !== "object") return null;
  const raw = value                           ;
  const id = nullablePositiveInteger(raw.id);
  return id === null ? null : { id, name: String(raw.name || `Tier ${id}`) };
}

function nullablePositiveInteger(value         )                {
  if (value === null || value === undefined || value === "") return null;
  const number = Number(value);
  return Number.isInteger(number) && number > 0 ? number : null;
}

function normaliseTokens(payload                         )         {
  if (!payload.access_token) throw new Error("OAuth gateway returned no access token");
  return {
    access_token: String(payload.access_token),
    refresh_token: String(payload.refresh_token || ""),
    token_type: String(payload.token_type || "Bearer"),
    expires_at: new Date(Date.now() + Number(payload.expires_in || 3600) * 1_000).toISOString(),
  };
}

async function abortableDelay(milliseconds        , signal              )                {
  await new Promise      ((resolve, reject) => {
    const timer = setTimeout(resolve, milliseconds);
    signal?.addEventListener("abort", () => { clearTimeout(timer); reject(signal.reason); }, { once: true });
  });
}


//# sourceURL=../src/auth.ts