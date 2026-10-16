
importScripts('./wasm_exec.js');
const go=new Go();
(async()=>{try{const response=await fetch('./engine.wasm');if(!response.ok)throw new Error('Engine download failed');const bytes=await response.arrayBuffer();const {instance}=await WebAssembly.instantiate(bytes,go.importObject);go.run(instance).catch(e=>postMessage({bootError:String(e)}));if(typeof publicDemo!=='function')throw new Error('Engine entrypoint missing');postMessage({ready:true});}catch(e){postMessage({bootError:String(e)});}})();
onmessage=({data})=>{try{const value=JSON.parse(publicDemo(JSON.stringify(data.input)));if(value.error)postMessage({id:data.id,error:value.error});else postMessage({id:data.id,value});}catch(e){postMessage({id:data.id,error:String(e)});}};
