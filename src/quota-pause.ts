import {createHash,randomUUID} from 'node:crypto';
import {mkdir,readFile,rename,unlink,writeFile} from 'node:fs/promises';
import {dirname,join} from 'node:path';
import type {SessionEvent} from './session.ts';
import type {AgentSettings} from './types.ts';
import type {Mode} from './permission.ts';

export function quotaIsAvailable(windows:unknown,now=Date.now()):boolean {
 if(!Array.isArray(windows))return false;
 return windows.every(window=>{
  if(!window||typeof window!=='object')return false;
  const used=Number(window.percent_settled??window.percent_used);
  if(!Number.isFinite(used)||used<0)return false;
  if(used<100)return true;
  const reset=Date.parse(window.resets_at||'');
  return Number.isFinite(reset)&&reset<=now;
 });
}

export interface QuotaCheckpoint {
 version:1;workspace:string;accountId:string;sessionId:string;events:SessionEvent[];
 settings:AgentSettings;mode:Mode;teamMode:boolean;teamExecute:boolean;workerModel:string;
 window?:string;resetsAt?:string;
}
export class QuotaCheckpointStore {
 readonly path:string;
 constructor(directory:string,readonlyWorkspace:string){this.path=join(directory,'quota-pauses',createHash('sha256').update(readonlyWorkspace).digest('hex')+'.json');}
 async save(value:QuotaCheckpoint):Promise<void>{await mkdir(dirname(this.path),{recursive:true,mode:0o700});const temporary=this.path+'.'+randomUUID()+'.tmp';await writeFile(temporary,JSON.stringify(value),{mode:0o600});await rename(temporary,this.path);}
 async load(workspace:string,accountId:string):Promise<QuotaCheckpoint|undefined>{let value:QuotaCheckpoint;try{value=JSON.parse(await readFile(this.path,'utf8'));}catch(error){if((error as NodeJS.ErrnoException).code==='ENOENT')return;throw error;}if(value.version!==1||value.workspace!==workspace||value.accountId!==accountId||!Array.isArray(value.events)||!['act','auto','plan'].includes(value.mode))throw new Error('Saved pause belongs to another workspace/account or is invalid.');return value;}
 async clear():Promise<void>{await unlink(this.path).catch(error=>{if(error.code!=='ENOENT')throw error;});}
}
