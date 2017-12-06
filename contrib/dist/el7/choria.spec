%define  debug_package %{nil}

Name: %{pkgname}
Version: %{version}
Release: %{iteration}%{choria_release}.%{dist}
Summary: The Choria Orchestrator Server
License: Apache-2.0
URL: https://choria.io
Group: System Tools
Source0: %{pkgname}-%{version}-%{choria_release}-Linux-amd64.tgz
Packager: R.I.Pienaar <rip@devco.net>
BuildRoot: %{_tmppath}/%{pkgname}-%{version}-%{release}-root-%(%{__id_u} -n)

%package broker
Summary: The Choria Orchestrator Middleware Broker
Requires: %{pkgname} = %{version}-%{release}
Group: System Tools

%description broker
The Choria Orchestrator Middleware Broker:

  * Middleware Broker
  * Federation Broker
  * Protocol Adapter Broker

%description
The Choria Orchestrator Server

%prep
%setup -q

%build
for i in server.service broker.service server.conf broker.conf choria-logrotate; do
  sed -i 's!{{pkgname}}!%{pkgname}!' dist/${i}
  sed -i 's!{{bindir}}!%{bindir}!' dist/${i}
  sed -i 's!{{etcdir}}!%{etcdir}!' dist/${i}
done


%install
rm -rf %{buildroot}
%{__install} -d -m0755  %{buildroot}/usr/lib/systemd/system
%{__install} -d -m0755  %{buildroot}/etc/logrotate.d
%{__install} -d -m0755  %{buildroot}%{bindir}
%{__install} -d -m0755  %{buildroot}%{etcdir}
%{__install} -d -m0755  %{buildroot}/var/log
%{__install} -m0644 dist/server.service %{buildroot}/usr/lib/systemd/system/%{pkgname}-server.service
%{__install} -m0644 dist/broker.service %{buildroot}/usr/lib/systemd/system/%{pkgname}-broker.service
%{__install} -m0644 dist/choria-logrotate %{buildroot}/etc/logrotate.d/%{pkgname}
%{__install} -m0640 dist/server.conf %{buildroot}%{etcdir}/server.conf
%{__install} -m0640 dist/broker.conf %{buildroot}%{etcdir}/broker.conf
%{__install} -m0755 choria-%{version}%{choria_release}-Linux-amd64 %{buildroot}%{bindir}/%{pkgname}

%clean
rm -rf %{buildroot}

%post broker
if [ $1 -eq 1 ] ; then
  systemctl --no-reload preset %{pkgname}-broker >/dev/null 2>&1 || :
fi

%post
if [ $1 -eq 1 ] ; then
  systemctl --no-reload preset %{pkgname}-server >/dev/null 2>&1 || :
fi

%preun broker
if [ $1 -eq 0 ] ; then
  systemctl --no-reload disable --now %{pkgname}-broker > /dev/null 2>&1 || :
fi

%preun
if [ $1 -eq 0 ] ; then
  systemctl --no-reload disable --now %{pkgname}-server > /dev/null 2>&1 || :
fi

%files
%config(noreplace)%{etcdir}/server.conf
%{bindir}/%{pkgname}
/etc/logrotate.d/%{pkgname}
/usr/lib/systemd/system/%{pkgname}-server.service

%files broker
%config(noreplace)%{etcdir}/broker.conf
/usr/lib/systemd/system/%{pkgname}-broker.service


%changelog
* Tue Dec 05 2017 R.I.Pienaar <rip@devco.net>
- Initial Release