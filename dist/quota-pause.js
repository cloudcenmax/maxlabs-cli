import {createHash,randomUUID} from 'node:crypto';
import {mkdir,readFile,rename,unlink,writeFile} from 'node:fs/promises';
import {dirname,join} from 'node:path';




export function quotaIsAvailable(windows        ,now=Date.now())         {
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






export class QuotaCheckpointStore {
          path       ;
 constructor(directory       ,readonlyWorkspace       ){this.path=join(directory,'quota-pauses',createHash('sha256').update(readonlyWorkspace).digest('hex')+'.json');}
 async save(value                )              {await mkdir(dirname(this.path),{recursive:true,mode:0o700});const temporary=this.path+'.'+randomUUID()+'.tmp';await writeFile(temporary,JSON.stringify(value),{mode:0o600});await rename(temporary,this.path);}
 async load(workspace       ,accountId       )                                   {let value                ;try{value=JSON.parse(await readFile(this.path,'utf8'));}catch(error){if((error                         ).code==='ENOENT')return;throw error;}if(value.version!==1||value.workspace!==workspace||value.accountId!==accountId||!Array.isArray(value.events)||!['act','auto','plan'].includes(value.mode))throw new Error('Saved pause belongs to another workspace/account or is invalid.');return value;}
 async clear()              {await unlink(this.path).catch(error=>{if(error.code!=='ENOENT')throw error;});}
}


//# sourceURL=../src/quota-pause.ts