// @vitest-environment node
import {readFileSync} from 'node:fs';
import {resolve} from 'node:path';
import {describe,expect,it} from 'vitest';

const stylesheet=readFileSync(resolve(process.cwd(),'src/style.css'),'utf8');
// These roles are consumed by the installed Material fields, selects, menus,
// lists, and dialogs. Keep an explicit list so removed overrides fail the check.
const neutralRoles=['surface','on-surface','on-surface-variant','surface-container','surface-container-high','surface-container-highest','outline','outline-variant','inverse-surface','inverse-on-surface','inverse-primary','secondary','on-error-container','scrim','shadow'];
function roles(css:string,theme:string):Record<string,string>{
  const selector=theme==='dark'?':root\\[data-theme="dark"\\]':':root';
  const body=new RegExp(selector+'\\s*\\{([^}]+)\\}').exec(css)?.[1]||'';
  const result=Object.fromEntries(Array.from(body.matchAll(/--md-sys-color-([a-z-]+):\s*(#[0-9a-f]+);/gi),match=>[match[1],match[2]]));
  for(const name of neutralRoles)if(!result[name])throw Error(`${theme} theme is missing ${name}`);
  return result;
}
function luminance(hex:string):number{
  const value=hex.slice(1);const expanded=value.length===3?value.split('').map(c=>c+c).join(''):value;
  const rgb=[0,2,4].map(index=>parseInt(expanded.slice(index,index+2),16)/255).map(value=>value<=.04045?value/12.92:((value+.055)/1.055)**2.4);
  return rgb[0]*.2126+rgb[1]*.7152+rgb[2]*.0722;
}
function contrast(first:string,second:string):number{const a=luminance(first),b=luminance(second);return(Math.max(a,b)+.05)/(Math.min(a,b)+.05);}
describe.each(['light','dark'])('%s theme neutral contrast',theme=>{
  it('declares every inherited neutral role used by Material controls',()=>expect(()=>roles(stylesheet,theme)).not.toThrow());
  it('keeps field labels and supporting text readable on every control surface',()=>{
    const colors=roles(stylesheet,theme);
    for(const background of ['surface','surface-container','surface-container-high','surface-container-highest'])for(const foreground of ['on-surface','on-surface-variant'])expect(contrast(colors[foreground],colors[background]),`${foreground} on ${background}`).toBeGreaterThanOrEqual(4.5);
  });
  it('keeps inverse text readable',()=>{const colors=roles(stylesheet,theme);expect(contrast(colors['inverse-on-surface'],colors['inverse-surface'])).toBeGreaterThanOrEqual(4.5);expect(contrast(colors['inverse-primary'],colors['inverse-surface'])).toBeGreaterThanOrEqual(4.5);});
  it('keeps active outlines distinguishable from field surfaces',()=>{const colors=roles(stylesheet,theme);expect(contrast(colors.outline,colors['surface-container'])).toBeGreaterThanOrEqual(3);});
  it('rejects removing the label-color override instead of inheriting a light default',()=>{const selector=theme==='dark'?/:root\[data-theme="dark"\]\s*\{[^}]+\}/:/:root\s*\{[^}]+\}/;const changed=stylesheet.replace(selector,block=>block.replace(/\s*--md-sys-color-on-surface-variant:[^;]+;/,''));expect(()=>roles(changed,theme)).toThrow('on-surface-variant');});
});
