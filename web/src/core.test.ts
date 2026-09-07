import { describe, expect, it } from 'vitest';
import { cronValid, esc, positive, query } from './core';
describe('UI request helpers',()=>{
 it('escapes untrusted host labels',()=>expect(esc(`<host&'\">`)).toBe('&lt;host&amp;&#39;&quot;&gt;'));
 it('builds encoded query values and omits undefined',()=>expect(query('/api',{hostId:'a b',all:true,skip:undefined})).toBe('/api?hostId=a%20b&all=true'));
 it('accepts only five field schedules',()=>{expect(cronValid('0 3 * * *')).toBe(true);expect(cronValid('0 3 * *')).toBe(false)});
 it('requires positive integer ports and durations',()=>{expect(positive('22')).toBe(true);expect(positive('0')).toBe(false);expect(positive('2.5')).toBe(false)});
});
