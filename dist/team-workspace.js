import { createHash, randomUUID } from 'node:crypto';
import { execFile } from 'node:child_process';
import { mkdir, readdir, readFile, stat, writeFile, rm, rename } from 'node:fs/promises';
import { resolve, dirname } from 'node:path';
import { promisify } from 'node:util';
import { safeTaskPath,                                } from './greenfield.js';

const hash=(raw           )       =>createHash('sha256').update(raw).digest('hex');
const omitted=new Set(['.git','.maxlabs','node_modules','vendor','.venv','venv','__pycache__','.next','.nuxt','.cache','.pytest_cache','.DS_Store','.aws','.ssh','.npmrc','.netrc','.pypirc','auth.json']);
export function workerEnvironment()                   {return Object.fromEntries(Object.entries(process.env).filter(([key])=>!/(TOKEN|SECRET|PASSWORD|API_KEY|ACCESS_KEY|PRIVATE_KEY|^HARNESS_|^MAXLABS_|^OPENAI_|^ANTHROPIC_|^SSH_AUTH_SOCK$)/i.test(key)));}
export async function sourceFiles(root       )                                {
 const files                      ={};let bytes=0;
 const walk=async(directory       ,prefix='')              =>{for(const entry of await readdir(directory,{withFileTypes:true})){
  if(omitted.has(entry.name)||entry.isSymbolicLink()||(entry.name!=='.env.example'&&(entry.name==='.env'||entry.name.startsWith('.env.'))))continue;
  const path=prefix+entry.name;if(!safeTaskPath(path))continue;const full=resolve(directory,entry.name);if(entry.isDirectory()){await walk(full,path+'/');continue;}
  if(!entry.isFile())throw new Error(`Unsupported source file ${path}`);bytes+=(await stat(full)).size;if(Object.keys(files).length>=5000||bytes>64*1024*1024)throw new Error('Source snapshot exceeds 5000 files or 64 MiB; narrow the project folder');files[path]=hash(await readFile(full));
 }};await walk(root);return files;
}
export async function git(root       ,args         ,signal             )                {
 const result=await promisify(execFile)('git',['-C',root,'-c','user.name=MaxLabs Team','-c','user.email=team@localhost','-c','commit.gpgsign=false','-c','core.hooksPath=/dev/null',...args],{env:{...workerEnvironment(),GIT_CONFIG_NOSYSTEM:'1',GIT_CONFIG_GLOBAL:process.platform==='win32'?'NUL':'/dev/null'},signal,maxBuffer:5*1024*1024});return result.stdout.trim();
}
export async function prepareExecution(store          ,state          ,signal             )                {
 if(state.execution?.applied_commit)delete state.execution;
 if(state.execution){const root=await store.path(state.execution.path);await git(root,['rev-parse','--is-inside-work-tree'],signal);return root;}
 const files=await sourceFiles(store.workspace.root),id=randomUUID(),snapshot=await store.path(`execution/${id}/snapshot`,true);await mkdir(snapshot,{recursive:true,mode:0o700});
 for(const path of Object.keys(files)){const from=await store.workspace.path(path),to=resolve(snapshot,path);await mkdir(dirname(to),{recursive:true});await writeFile(to,await readFile(from),{mode:(await stat(from)).mode&0o777});}
 await git(snapshot,['init','-b','baseline'],signal);await git(snapshot,['add','--all','--force'],signal);await git(snapshot,['commit','--allow-empty','-m','Project baseline before team execution'],signal);
 const base=await git(snapshot,['rev-parse','HEAD'],signal),path=`execution/${id}/worktree`,root=await store.path(path,true),branch=`team-${id}`;await git(snapshot,['worktree','add','-b',branch,root,base],signal);
 state.execution={path,branch,base_commit:base,base_files:files,commit:base};await store.persist(state);return root;
}
export async function checkpoint(root       ,message       ,signal             )                {
 const paths=Object.keys(await sourceFiles(root)).sort();await git(root,['add','--update'],signal);for(let start=0;start<paths.length;start+=100)await git(root,['add','--force','--',...paths.slice(start,start+100)],signal);
 await git(root,['commit','--allow-empty','-m',message],signal);return git(root,['rev-parse','HEAD'],signal);
}
export function sameFiles(a                      ,b                      )        {return Object.keys(a).length===Object.keys(b).length&&Object.entries(a).every(([path,hash])=>b[path]===hash);}
export async function applyExecution(store          ,state          )              {
 const execution=state.execution;if(!execution)return;const root=await store.path(execution.path),files=await sourceFiles(root);
 if(!execution.verified_files||!sameFiles(files,execution.verified_files))throw new Error('Build workspace changed after QA; run independent verification again before accepting');
 if(execution.applied_commit===execution.commit)return;
 const original=await sourceFiles(store.workspace.root);if(!sameFiles(original,execution.base_files))throw new Error('Original project changed since the build snapshot; reconcile these edits before accepting this release');
 const changed=new Set([...Object.keys(files),...Object.keys(execution.base_files)].filter(path=>files[path]!==execution.base_files[path]));
 for(const path of changed){const target=await store.workerPath(path);let current='missing';try{current=hash(await readFile(target));}catch(e){if((e                         ).code!=='ENOENT')throw e;}if(current!==(execution.base_files[path]||'missing'))throw new Error(`Original project changed at ${path}; review and reconcile it before accepting this release`);}
 for(const path of changed){const target=await store.workerPath(path);if(!(path in files)){await rm(target);continue;}await mkdir(dirname(target),{recursive:true});const temporary=target+`.team-apply-${randomUUID()}`;await writeFile(temporary,await readFile(resolve(root,path)),{mode:(await stat(resolve(root,path))).mode&0o777});try{await rename(temporary,target);}catch(error){await rm(temporary,{force:true});throw error;}}
 execution.applied_commit=execution.commit;
}


//# sourceURL=../src/team-workspace.ts