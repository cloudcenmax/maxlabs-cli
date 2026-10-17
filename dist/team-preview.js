import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { createServer,             } from "node:http";



export const previewPolicy = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; font-src data:; form-action 'none'; base-uri 'none'; sandbox allow-scripts";
export function openSystemBrowser(url        )                {
  const [command, ...args] = process.platform === "darwin" ? ["open", url] : process.platform === "win32" ? ["rundll32.exe", "url.dll,FileProtocolHandler", url] : ["xdg-open", url];
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { stdio: "ignore", shell: false });
    const timeout = setTimeout(() => { child.unref(); reject(new Error("Browser launch timed out")); }, 5000);
    child.once("error", error => { clearTimeout(timeout); reject(error); });
    child.once("exit", code => { clearTimeout(timeout); code === 0 ? resolve() : reject(new Error("Browser launcher exited with code " + code)); });
  });
}
/** An isolated local origin; never serves project files, credentials, or workflow controls. */
export class TeamPreview {
  #server         ;
  #html = "";
  #url = "";
  #path = "/" + randomBytes(24).toString("hex");
           store           ;
           launch                                ;
  constructor(store           , launch = openSystemBrowser) { this.store = store; this.launch = launch; }
  async show()                                            {
    const status = await this.store.load(), design = status.artifacts.find(a => a.kind === "design");
    if (!design) throw new Error("No design yet. Continue discovery to prepare a preview.");
    const content = await this.store.content(design);
    if (typeof content.prototype_html !== "string" || !content.prototype_html.trim()) throw new Error("The design has no preview HTML.");
    this.#html = content.prototype_html;
    if (!this.#server) {
      const server = createServer((request, response) => {
        // A sandboxed prototype can run its own UI, but cannot navigate to another local resource.
        if (request.url !== this.#path || !["GET", "HEAD"].includes(request.method || "")) { response.writeHead(404); response.end(); return; }
        response.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Content-Security-Policy": previewPolicy, "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer" });
        response.end(request.method === "HEAD" ? undefined : this.#html);
      });
      await new Promise      ((resolve, reject) => { server.once("error", reject); server.listen(0, "127.0.0.1", () => { server.removeListener("error", reject); resolve(); }); });
      server.unref(); this.#server = server;
      this.#url = `http://127.0.0.1:${(server.address()               ).port}${this.#path}`;
    }
    try { await this.launch(this.#url); return { url: this.#url, opened: true }; }
    catch { return { url: this.#url, opened: false }; }
  }
  close()       { this.#server?.close(); this.#server?.closeAllConnections(); this.#server = undefined; }
}


//# sourceURL=../src/team-preview.ts