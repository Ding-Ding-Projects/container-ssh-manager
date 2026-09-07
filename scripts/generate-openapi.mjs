import {readFileSync,writeFileSync} from 'node:fs';
const spec={openapi:'3.1.0',info:{title:'Container SSH Manager API',version:'0.1.0',description:'Authenticated single-owner administration. See adjacent API articles for operation-specific request shapes and interruption semantics.'},servers:[{url:'/'}],security:[{ownerSession:[]}],components:{securitySchemes:{ownerSession:{type:'apiKey',in:'cookie',name:'manager_session'}}},paths:{}};
for(const file of ['connections','engine','jobs']){
 const text=readFileSync(new URL(`../docs/api-${file}.md`,import.meta.url),'utf8');
 for(const line of text.split('\n')){
  const match=line.match(/^\|\s*`?(GET|POST|PUT|DELETE|PATCH)`?\s*\|\s*`([^`]+)`/);if(!match)continue;
  const method=match[1].toLowerCase(),path=match[2].split('?')[0];
  const parameters=[...path.matchAll(/\{([^}]+)\}/g)].map(m=>({name:m[1],in:'path',required:true,schema:{type:'string'}}));
  if(match[2].includes('hostId='))parameters.push({name:'hostId',in:'query',required:true,schema:{type:'string'}});
  const op={tags:[file],summary:`${match[1]} ${path}`,parameters,responses:{'200':{description:'Operation result; see API article for exact representation.'},'202':{description:'Accepted durable operation; poll its returned identifier.'},'400':{description:'Invalid request'},'401':{description:'Owner sign-in required'},'403':{description:'Origin rejected'},'409':{description:'Conflict or changed state'},'502':{description:'Host or engine operation unavailable'}}};
  if(['post','put','patch'].includes(method))op.requestBody={required:false,content:{'application/json':{schema:{type:'object',description:`See docs/api-${file}.md for this operation's documented fields.`}}}};
  (spec.paths[path]??={})[method]=op;
 }
}
spec.paths['/api/v1/login']={post:{security:[],summary:'Create owner session',requestBody:{required:true,content:{'application/json':{schema:{type:'object',required:['password'],properties:{password:{type:'string',writeOnly:true}},additionalProperties:false}}}},responses:{'200':{description:'Secure session cookie created'},'401':{description:'Sign-in failed'},'429':{description:'Retry later'}}}};
for(const path of ['health','version'])spec.paths[`/api/v1/${path}`]={get:{security:[],summary:path,responses:{'200':{description:path==='version'?'Recorded version and build start time, or explicit unavailable values':'Service liveness'}}}};
writeFileSync(new URL('../docs/openapi.json',import.meta.url),JSON.stringify(spec,null,2)+'\n');
console.log(`Generated ${Object.keys(spec.paths).length} documented API paths`);
