const fs=require('fs');const {JSDOM}=require('jsdom');
const input=JSON.parse(fs.readFileSync(0,'utf8'));const out=[];
const dom=new JSDOM('<!DOCTYPE html><html><head></head><body><div id="root"></div></body></html>',{url:input.pageURL||'https://www.dola.com/chat/',runScripts:'outside-only',pretendToBeVisual:true});
const w=dom.window;w.console={log(){},error(){},warn(){},info(){},debug(){}};
for(const [k,v] of Object.entries({userAgent:input.userAgent,language:input.language||'en-US',languages:input.languages||['en-US','en'],platform:input.platform||'Linux x86_64',hardwareConcurrency:input.hardwareConcurrency||8})){if(v!==undefined)Object.defineProperty(w.navigator,k,{value:v,configurable:true})}
for(const [k,v] of Object.entries(input.localStorage||{}))w.localStorage.setItem(k,String(v));
for(const [k,v] of Object.entries(input.sessionStorage||{}))w.sessionStorage.setItem(k,String(v));
for(const p of (input.cookie||'').split(';'))if(p.includes('='))w.document.cookie=p.trim()+'; path=/; secure';
w.Request=Request;w.Response=Response;w.Headers=Headers;w.TextEncoder=TextEncoder;w.TextDecoder=TextDecoder;
w.navigator.sendBeacon=()=>true;
w.fetch=async function(value,opts){out.push({url:String(value?.url||value),body:opts?.body,headers:opts?.headers});return new Response('{}',{status:200,headers:{'Content-Type':'application/json'}})};
w.XMLHttpRequest.prototype.open=function(method,url){this._url=url};w.XMLHttpRequest.prototype.setRequestHeader=function(){};w.XMLHttpRequest.prototype.send=function(){};
try{
w.eval(fs.readFileSync(__dirname+'/bdms-sdk.f36aabd9.js','utf8'));
const modules=w.__LOADABLE_LOADED_CHUNKS__[0][1],cache={};function req(id){if(cache[id])return cache[id].exports;const m={exports:{}};cache[id]=m;modules[id](m,m.exports,req);return m.exports}
req(633286);w.bdms.init({aid:495671,pageId:26930,paths:{include:['/alice','/samantha','/passport','/biz','/chat/completion','/chat/async/chunk_stream'],exclude:['/samantha/notice/info','/samantha/user/preference/get','/samantha/user/ab/get','/samantha/plugin/recommend/webtodesktop','/samantha/guidance/get_task','/samantha/guidance/draw_result']},ic:13,ddrt:13});
w.fetch(input.url,{method:'POST',body:input.body,headers:input.headers||{'Content-Type':'application/json'}}).then(()=>{process.stdout.write(JSON.stringify(out.at(-1)));dom.window.close();process.exit(0)}).catch(()=>{process.stderr.write('signing failed');dom.window.close();process.exit(1)});
}catch(e){process.stderr.write('sdk init failed: '+e.name);dom.window.close();process.exit(1)}
setTimeout(()=>{process.stderr.write('signing timeout');process.exit(1)},10000).unref();
