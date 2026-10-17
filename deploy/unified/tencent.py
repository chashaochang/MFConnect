#!/usr/bin/env python3
"""Tencent Cloud DNSPod transport; credentials remain on the cloud server."""
import datetime,hashlib,hmac,json,os,time,urllib.request

def call(action,body):
    stamp=int(time.time());date=datetime.datetime.fromtimestamp(stamp,datetime.timezone.utc).strftime('%Y-%m-%d')
    raw=json.dumps(body,separators=(',',':')).encode();host='dnspod.tencentcloudapi.com'
    headers='content-type:application/json\nhost:'+host+'\nx-tc-action:'+action.lower()+'\n'
    signed='content-type;host;x-tc-action'
    canonical='POST\n/\n\n'+headers+'\n'+signed+'\n'+hashlib.sha256(raw).hexdigest()
    scope=date+'/dnspod/tc3_request'
    message='TC3-HMAC-SHA256\n'+str(stamp)+'\n'+scope+'\n'+hashlib.sha256(canonical.encode()).hexdigest()
    sign=lambda key,value:hmac.new(key,value.encode(),hashlib.sha256).digest()
    key=sign(sign(sign(('TC3'+os.environ['Tencent_SecretKey']).encode(),date),'dnspod'),'tc3_request')
    signature=hmac.new(key,message.encode(),hashlib.sha256).hexdigest()
    authorization='TC3-HMAC-SHA256 Credential='+os.environ['Tencent_SecretId']+'/'+scope+', SignedHeaders='+signed+', Signature='+signature
    request=urllib.request.Request('https://'+host,data=raw,headers={'Content-Type':'application/json','Host':host,'X-TC-Action':action,'X-TC-Timestamp':str(stamp),'X-TC-Version':'2021-03-23','Authorization':authorization})
    with urllib.request.urlopen(request,timeout=25) as response:result=json.load(response)['Response']
    if 'Error' in result:
        if result['Error']['Code']=='ResourceNotFound.NoDataOfRecord':return {'RecordList':[]}
        raise RuntimeError(result['Error']['Code'])
    return result
