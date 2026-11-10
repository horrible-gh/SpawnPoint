from pathlib import Path
p=Path('.')
f=p/'internal/access/admin.go';s=f.read_text()
s=s.replace('!textOK(u.Login, 128) || !textOK(u.Name, 128) || strings.ContainsAny(u.Login, " \\t") || logins[strings.ToLower(u.Login)] ||', '!textOK(u.Login, 256) || !textOK(u.Name, 128) ||')
s=s.replace('''		logins[strings.ToLower(u.Login)] = true
		if u.Auth == "local" {
''','''		if u.Auth == "local" {
			if !textOK(u.Login, 128) || strings.ContainsAny(u.Login, " \\t") || logins[strings.ToLower(u.Login)] {
				return errors.New("사용자 ID가 없거나 중복되었습니다")
			}
			logins[strings.ToLower(u.Login)] = true
''')
s=s.replace('u.Subject == "" || u.External == "" || u.Hash != "" || subjects[u.Subject]', 'u.Subject == "" || strings.TrimSpace(u.External) == "" || !textOK(u.External, 256) || u.Hash != "" || subjects[u.Subject]')
s=s.replace('''		} else if u.Auth == "os" {
			u.Subject, err = m.resolveOS(u.External)
''','''		} else if u.Auth == "os" {
			// OS authentication uses External + the resolved SID/UID, not a
			// separately chosen login. Keep legacy labels stable on edits; new
			// links derive their label from the OS account. ID remains generated.
			u.Login = u.External
			if old != nil && old.Auth == "os" {
				u.Login = old.Login
			}
			if u.External == "" || !textOK(u.External, 256) {
				return bad("서버에서 확인 가능한 OS 계정을 입력하세요")
			}
			u.Subject, err = m.resolveOS(u.External)
''')
f.write_text(s)
f=p/'internal/webui/access.html';s=f.read_text()
s=s.replace('function subjectInfo(key)', "function userIdentity(u){return u?(u.auth==='os'?u.external:u.login):'';}\nfunction subjectInfo(key)")
s=s.replace("sub:u?.login||'사용자'", "sub:userIdentity(u)||'사용자'")
s=s.replace("esc(user.login.slice(0,2).toUpperCase())", "esc(userIdentity(user).slice(0,2).toUpperCase())")
s=s.replace("esc(u.login.slice(0,2).toUpperCase())", "esc(userIdentity(u).slice(0,2).toUpperCase())")
s=s.replace("(u.name+' '+u.login).toLowerCase()", "(u.name+' '+userIdentity(u)).toLowerCase()")
s=s.replace('placeholder="이름 또는 사용자 ID"', 'placeholder="이름 또는 계정"')
s=s.replace("${esc(u.login)}</small>", "${esc(userIdentity(u))}</small>")
s=s.replace("${esc(u.login)}</p>", "${esc(userIdentity(u))}</p>")
s=s.replace("esc(db.users.find(u=>u.id===t.owner)?.login||'삭제된 사용자')", "esc(userIdentity(db.users.find(u=>u.id===t.owner))||'삭제된 사용자')")
start=s.index('function userModal(id){');end=s.index('function userGroupModal(id)',start)
chunk=s[start:end]
chunk=chunk.replace('<div class="field"><label for="userName">', '<div class="field" id="userNameField"><label for="userName">')
chunk=chunk.replace('<div class="field"><label for="userLogin">', '<div class="field" id="userLoginField"><label for="userLogin">')
chunk=chunk.replace('${esc(u.login)}" required pattern', '${esc(u.auth===\'local\'?u.login:\'\')}" required pattern')
chunk=chunk.replace("${id?'readonly':''}", "${id&&u.auth==='local'?'readonly':''}")
chunk=chunk.replace("'DOMAIN\\username 또는 .\\username'", "'DOMAIN\\\\username 또는 .\\\\username'")
chunk=chunk.replace("'user',{id:u.id,icon:'user'});", "'user',{id:u.id,originalAuth:u.auth,icon:'user'});\n syncUserAuthFields();")
chunk+='''function syncUserAuthFields(){
 const os=$('#userAuth').value==='os',login=$('#userLogin'),external=$('#externalIdentity'),password=$('#userTempPassword');
 $('#userLoginField').hidden=os;login.disabled=os;login.required=!os;
 $('#userNameField').classList.toggle('full',os);
 $('#externalField').hidden=!os;external.disabled=!os;external.required=os;
 $('#localPasswordField').hidden=os;password.disabled=os;
 password.required=!os&&(!modalState.id||modalState.originalAuth!=='local');
}
'''
s=s[:start]+chunk+s[end:]
s=s.replace("if(t.id==='userAuth'){$('#externalField').hidden=t.value!=='os';$('#localPasswordField').hidden=t.value!=='local';}", "if(t.id==='userAuth')syncUserAuthFields();")
s=s.replace("case 'user':await adminRequest({action:'user.upsert',revision,user:{id:m.id||'',login:String(form.get('login')||'').trim(),", "case 'user':await adminRequest({action:'user.upsert',revision,user:{id:m.id||'',...(form.get('auth')==='local'?{login:String(form.get('login')||'').trim()}:{}),")
f.write_text(s)
